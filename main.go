package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/gagliardetto/solana-go"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	"github.com/montanaflynn/stats"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"golang.org/x/time/rate"
)

const (
	ComputeUnitLimit = 30000
)

var Version string = "development"

var (
	DEFAULT_CONFIG = Config{
		RpcUrl:    "http://node.foo.cc",
		RateLimit: 200,
		TxCount:   100,
		PrioFee:   0,
	}

	TestID string

	// variable for the log file; set to benchmark.log as a fallback
	LogFileName string = "benchmark.log"

	GlobalConfig *Config
	TestAccount  *solana.PrivateKey

	wg sync.WaitGroup
	mu sync.RWMutex

	// the rate limiter
	Limiter = rate.NewLimiter(rate.Limit(200), 200)

	// the time the test should end
	StopTime time.Time

	// the number of transactions sent and transactions that landed
	SentTransactions      uint64
	ProcessedTransactions uint64

	// transaction send times
	TxTimes = make(map[solana.Signature]time.Time)

	// delta between transaction send times and landing times
	TxDeltas = []time.Duration{}

	// blocks where transactions landed
	TxBlocks = make(map[uint64]uint64)

	WsListener *WebsocketListener

	SimpleLogger *log.Logger
)

type Config struct {
	PrivateKey  string  `json:"private_key"`
	RpcUrl      string  `json:"rpc_url"`
	WsUrl       string  `json:"ws_url"`
	SendRpcUrl  string  `json:"send_rpc_url"`
	RateLimit   uint64  `json:"rate_limit"`
	TxCount     uint64  `json:"tx_count"`
	PrioFee     float64 `json:"prio_fee"`
	NodeRetries uint    `json:"node_retries"`
}

func (c *Config) GetWsUrl() string {
	if c.WsUrl != "" {
		return c.WsUrl
	}

	// replace http:// with ws:// and https:// with wss://
	return strings.ReplaceAll(strings.ReplaceAll(c.RpcUrl, "http://", "ws://"), "https://", "wss://")
}

func (c *Config) GetSendUrl() string {
	if c.SendRpcUrl != "" {
		return c.SendRpcUrl
	}

	return c.RpcUrl
}

type WebsocketListener struct {
	Subscription *ws.LogSubscription
	Listening    bool
}

func (l *WebsocketListener) Start() {
	wsClient, err := ws.Connect(context.TODO(), GlobalConfig.GetWsUrl())
	if err != nil {
		log.Fatalf("error connecting to websocket: %v", err)
	}

	defer wg.Done()

	// invoke the default stop timer
	time.AfterFunc(time.Until(StopTime), WsListener.Stop)

	l.Subscription, err = wsClient.LogsSubscribeMentions(TestAccount.PublicKey(), rpc.CommitmentProcessed)
	if err != nil {
		log.Fatalf("error subscribing to logs: %v", err)
	}
	l.Listening = true

	log.Info("Listening for transactions...")

	// start sending transactions now that the websocket is ready
	SendTransactions()

	for l.Listening {
		got, err := l.Subscription.Recv()
		if err != nil {
			log.Error(err.Error())
		}

		if got == nil || got.Value.Err != nil {
			continue
		}

		re := regexp.MustCompile(`memobench:.*?(\d+).*\[(.*?)\]`)
		for _, line := range got.Value.Logs {
			matches := re.FindStringSubmatch(line)
			if len(matches) != 3 {
				continue
			}
			testNum, id := matches[1], matches[2]

			if id != TestID {
				log.Warn(
					"Received unexpected test ID",
					"num", testNum,
					"id", id,
					"sig", got.Value.Signature.String(),
				)
				continue
			}

			var delta time.Duration
			mu.Lock()
			// record the time delta
			txSendTime, found := TxTimes[got.Value.Signature]
			if found {
				ProcessedTransactions += 1
				delta = time.Since(txSendTime)
				TxDeltas = append(TxDeltas, delta)

				// record the block where the tx landed
				// add new entry if needed
				if _, ok := TxBlocks[got.Context.Slot]; !ok {
					TxBlocks[got.Context.Slot] = 0
				}

				// increment the tx count for this block
				TxBlocks[got.Context.Slot] += 1
			}

			mu.Unlock()

			// skip this tx if it's not in the TxTimes map
			// this could happen if the test was restarted and a tx from a previous test landed
			if !found {
				continue
			}

			log.Info(
				"Tx Processed",
				"num", testNum,
				"sig", got.Value.Signature.String(),
				"delta", delta.Truncate(time.Millisecond).String(),
				"landed", fmt.Sprintf("%d/%d", ProcessedTransactions, SentTransactions),
			)

			if ProcessedTransactions >= SentTransactions {
				l.Stop()
			}
			break
		}
	}

	log.Info("Stopping listening for log events...")
}

func (l *WebsocketListener) Stop() {
	if !l.Listening {
		return
	}

	l.Listening = false
	l.Subscription.Unsubscribe()
}

func SetupLogger() {
	LogFileName = fmt.Sprintf("memobench_%d_%s.log", time.Now().UnixMilli(), TestID)
	logFile, err := os.OpenFile(LogFileName, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		log.Fatalf("error opening file: %v", err)
	}

	multi := io.MultiWriter(os.Stdout, logFile)

	// create a simplified logger for logging the test results
	SimpleLogger = log.NewWithOptions(multi, log.Options{
		ReportTimestamp: false,
	})

	// set the default logger for logging during the test
	log.SetDefault(log.NewWithOptions(multi, log.Options{
		Prefix:          TestID,
		ReportTimestamp: true,
		TimeFunction:    func(time.Time) time.Time { return time.Now().UTC() },
		TimeFormat:      "15:04:05.0000",
	}))
}

func ReadConfig() *Config {
	data, err := os.ReadFile("config.json")
	if err != nil {
		// if the error is that the file doesn't exist, create it, and exit
		if os.IsNotExist(err) {
			if err := WriteConfig(&DEFAULT_CONFIG); err != nil {
				log.Fatalf("error creating config file: %v", err)
			}

			log.Info("config file saved, edit the config and restart")
			os.Exit(0)
		}

		log.Fatalf("error opening config file: %v", err)
	}

	var out Config

	err = json.Unmarshal(data, &out)
	if err != nil {
		log.Fatalf("error parsing config file: %v", err)
	}

	return &out
}

func WriteConfig(config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		log.Fatalf("error saving config file: %v", err)
	}

	return os.WriteFile("config.json", data, 0644)
}

func VerifyPrivateKey(base58key string) {
	account, err := solana.PrivateKeyFromBase58(base58key)
	if err != nil {
		log.Fatalf("error parsing private key: %v", err)
	}
	TestAccount = &account
}

func AssertSufficientBalance() {
	// Create a new RPC client:
	rpcClient := rpc.New(GlobalConfig.RpcUrl)

	// fetch the latest blockhash
	balance, err := rpcClient.GetBalance(context.TODO(), TestAccount.PublicKey(), rpc.CommitmentFinalized)
	if err != nil || balance == nil {
		log.Fatalf("error getting test wallet balance: %v", err)
	}

	costPerTx := uint64(GlobalConfig.PrioFee*ComputeUnitLimit + 5000)
	totalCost := GlobalConfig.TxCount * costPerTx

	// abort if balance is less than 50% of the maximum cost
	if balance.Value < totalCost/2 {
		log.Fatal(
			"Insufficient balance in test wallet.",
			"balance", fmt.Sprintf("%.6f SOL", float64(balance.Value)/float64(solana.LAMPORTS_PER_SOL)),
			"required", fmt.Sprintf("%.6f SOL", float64(totalCost)/float64(solana.LAMPORTS_PER_SOL)),
		)
	}
}

func SendTransactions() {
	// Create a new RPC client:
	rpcClient := rpc.New(GlobalConfig.RpcUrl)

	// create the send client
	sendClient := rpc.New(GlobalConfig.GetSendUrl())

	// fetch the latest blockhash
	recent, err := rpcClient.GetLatestBlockhash(context.TODO(), rpc.CommitmentFinalized)
	if err != nil {
		log.Fatalf("error getting recent blockhash: %v", err)
	}

	// save current time and set the experiment end time
	// hash expire after 150 blocks, each block is about 400ms
	// we use 160 blocks just out of abundance of caution
	StopTime = time.Now().Add(160 * 400 * time.Millisecond)
	time.AfterFunc(time.Until(StopTime), WsListener.Stop)

	for i := uint64(0); i < GlobalConfig.TxCount; i++ {
		go func(id uint64) {
			instructions := []solana.Instruction{}

			if GlobalConfig.PrioFee > 0 {
				instructions = append(instructions, computebudget.NewSetComputeUnitPriceInstruction(uint64(GlobalConfig.PrioFee*1e6)).Build())
				instructions = append(instructions, computebudget.NewSetComputeUnitLimitInstruction(ComputeUnitLimit).Build())
			}

			instructions = append(instructions, solana.NewInstruction(
				solana.MemoProgramID,
				solana.AccountMetaSlice{
					solana.NewAccountMeta(TestAccount.PublicKey(), false, true),
				},
				[]byte(fmt.Sprintf("memobench: Test %d [%s]", id, TestID)),
			))

			tx, err := solana.NewTransaction(
				instructions,
				recent.Value.Blockhash,
				solana.TransactionPayer(TestAccount.PublicKey()),
			)
			if err != nil {
				log.Fatalf("error creating new transaction: %v", err)
			}

			_, err = tx.Sign(
				func(key solana.PublicKey) *solana.PrivateKey {
					if TestAccount.PublicKey().Equals(key) {
						return TestAccount
					}
					return nil
				},
			)
			if err != nil {
				log.Fatalf("error signing new transaction: %v", err)
			}

			// sleep until the next xx:xx:10s; then start spamming the transactions
			startTime := time.Now().Truncate(5 * time.Second).Add(10 * time.Second)
			sleepTime := time.Until(startTime)

			// only log the first time, to avoid spamming logs
			if id == 1 {
				log.Info("Threads sleeping until starting spam", "delay", sleepTime.Truncate(time.Millisecond))
			}

			time.Sleep(sleepTime)

			t0 := time.Now()
			if err := Limiter.Wait(context.TODO()); err != nil {
				log.Error(err.Error())
				return
			}

			// log if the thread had to throttle to keep under the rate limit
			throttleTime := time.Since(t0).Truncate(time.Millisecond)
			if throttleTime > 0 {
				log.Info("Thread throttled to respect rate-limit, Sending now", "thread", id, "delay", throttleTime)
			}

			log.Infof("Sending Tx [%s]", tx.Signatures[0])

			sig, err := sendClient.SendTransactionWithOpts(
				context.TODO(),
				tx,
				rpc.TransactionOpts{
					Encoding:      solana.EncodingBase64,
					SkipPreflight: true,
					MaxRetries:    &GlobalConfig.NodeRetries,
				},
			)
			if err != nil {
				if val, ok := err.(*jsonrpc.RPCError); ok {
					log.Errorf("Error sending tx: Received RPC error: %s", val.Message)
					return
				}

				log.Errorf("Error sending tx: %v", err)
				return
			}

			// save the tx send time for later comparison
			mu.Lock()
			TxTimes[sig] = time.Now()
			SentTransactions += 1
			mu.Unlock()
		}(i + 1)
	}
}

func DisplayBlocks() {
	// find the first & last blocks
	// and the block with the most transactions
	var first uint64 = math.MaxUint64
	var last uint64
	var top uint64

	for block, count := range TxBlocks {
		first = uint64(math.Min(float64(first), float64(block)))
		last = uint64(math.Max(float64(last), float64(block)))
		top = uint64(math.Max(float64(top), float64(count)))
	}

	for block := first; block <= last; block++ {
		count, ok := TxBlocks[block]
		if !ok {
			SimpleLogger.Printf("Block %s : %3d", message.NewPrinter(language.English).Sprintf("%d", block), count)
			continue
		}

		// deduce the # of * characters to display
		// use math.Ceil to round up to ensure we don't display 0 * characters
		// (only for blocks with > 0 transactions)
		stars := math.Ceil(float64(count) / float64(ProcessedTransactions) * 100)

		SimpleLogger.Printf("Block %s : %3d | %5.1f%% | %s",
			message.NewPrinter(language.English).Sprintf("%d", block),
			count,
			float64(count)/float64(ProcessedTransactions)*100,
			strings.Repeat("*", int(stars)),
		)
	}
}

func main() {
	fmt.Println("                                                                                   ")
	fmt.Println(" ███╗   ███╗███████╗███╗   ███╗ ██████╗ ██████╗ ███████╗███╗   ██╗ ██████╗██╗  ██╗ ")
	fmt.Println(" ████╗ ████║██╔════╝████╗ ████║██╔═══██╗██╔══██╗██╔════╝████╗  ██║██╔════╝██║  ██║ ")
	fmt.Println(" ██╔████╔██║█████╗  ██╔████╔██║██║   ██║██████╔╝█████╗  ██╔██╗ ██║██║     ███████║ ")
	fmt.Println(" ██║╚██╔╝██║██╔══╝  ██║╚██╔╝██║██║   ██║██╔══██╗██╔══╝  ██║╚██╗██║██║     ██╔══██║ ")
	fmt.Println(" ██║ ╚═╝ ██║███████╗██║ ╚═╝ ██║╚██████╔╝██████╔╝███████╗██║ ╚████║╚██████╗██║  ██║ ")
	fmt.Println(" ╚═╝     ╚═╝╚══════╝╚═╝     ╚═╝ ╚═════╝ ╚═════╝ ╚══════╝╚═╝  ╚═══╝ ╚═════╝╚═╝  ╚═╝ ")
	fmt.Printf("%82s", Version)
	fmt.Println()
	fmt.Println()

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c

		fmt.Println()
		log.Info("CTRL+C detected, Force stopping the test")
		fmt.Println()

		// if the websocket is not listening, exit immediately
		// no need to call stop and log the test results
		if !WsListener.Listening {
			os.Exit(0)
		}

		WsListener.Stop()
	}()

	// generate the test id
	randomBytes := make([]byte, 4)
	_, err := rand.Read(randomBytes)
	if err != nil {
		panic(err)
	}

	TestID = hex.EncodeToString(randomBytes)

	// set up logger
	SetupLogger()

	// read the config file
	GlobalConfig = ReadConfig()

	// verify the private key is valid
	VerifyPrivateKey(GlobalConfig.PrivateKey)

	// set the rate limit
	Limiter.SetLimit(rate.Limit(GlobalConfig.RateLimit))
	Limiter.SetBurst(int(GlobalConfig.RateLimit))

	SimpleLogger.Printf("Date                : %s", time.Now().UTC().Format(time.RFC1123))
	SimpleLogger.Printf("Test Wallet         : %s", TestAccount.PublicKey().String())
	SimpleLogger.Printf("Starting Test ID    : %s", TestID)
	SimpleLogger.Printf("RPC URL             : %s", GlobalConfig.RpcUrl)
	SimpleLogger.Printf("WS URL              : %s", GlobalConfig.GetWsUrl())
	SimpleLogger.Printf("RPC Send URL        : %s", GlobalConfig.GetSendUrl())
	SimpleLogger.Printf("Transaction Count   : %d", GlobalConfig.TxCount)
	SimpleLogger.Printf("Rate Limit          : %d", GlobalConfig.RateLimit)
	SimpleLogger.Printf("Priority Fee/CU     : %f Lamports (%.9f SOL)", GlobalConfig.PrioFee, (GlobalConfig.PrioFee*ComputeUnitLimit+5000)/float64(solana.LAMPORTS_PER_SOL))
	SimpleLogger.Printf("Node Retries        : %d", GlobalConfig.NodeRetries)
	SimpleLogger.Printf("")

	// verify test wallet balance
	AssertSufficientBalance()

	// start the websocket listener
	wg.Add(1)
	WsListener = new(WebsocketListener)
	go WsListener.Start()
	wg.Wait()

	SimpleLogger.Printf("")
	SimpleLogger.Printf("Finished Test ID       : %s", TestID)
	SimpleLogger.Printf("RPC URL                : %s", GlobalConfig.RpcUrl)
	SimpleLogger.Printf("WS URL                 : %s", GlobalConfig.GetWsUrl())
	SimpleLogger.Printf("RPC Send URL           : %s", GlobalConfig.GetSendUrl())
	SimpleLogger.Printf("Transaction Count      : %d", GlobalConfig.TxCount)
	SimpleLogger.Printf("Rate Limit             : %d", GlobalConfig.RateLimit)
	SimpleLogger.Printf("Priority Fee/CU        : %f Lamports (%.9f SOL)", GlobalConfig.PrioFee, (GlobalConfig.PrioFee*ComputeUnitLimit+5000)/float64(solana.LAMPORTS_PER_SOL))
	SimpleLogger.Printf("Node Retries           : %d", GlobalConfig.NodeRetries)
	SimpleLogger.Printf("Transactions Landed    : %d/%d (%.1f%%)", ProcessedTransactions, SentTransactions, float64(ProcessedTransactions)/float64(SentTransactions)*100.0)

	// calculate landing time results, if there was any
	if len(TxDeltas) > 0 {
		var landingTimes []float64
		for _, v := range TxDeltas {
			landingTimes = append(landingTimes, float64(v.Nanoseconds()))
		}

		minDelta, _ := stats.Min(landingTimes)
		maxDelta, _ := stats.Max(landingTimes)
		avg, _ := stats.Mean(landingTimes)
		median, _ := stats.Median(landingTimes)
		p90, _ := stats.Percentile(landingTimes, 90)
		p95, _ := stats.Percentile(landingTimes, 95)
		p99, _ := stats.Percentile(landingTimes, 99)

		SimpleLogger.Printf("Min Tx Landing Time    : %s", (time.Duration(minDelta)).Truncate(time.Millisecond))
		SimpleLogger.Printf("Max Tx Landing Time    : %s", (time.Duration(maxDelta)).Truncate(time.Millisecond))
		SimpleLogger.Printf("Avg Tx Landing Time    : %s", (time.Duration(avg)).Truncate(time.Millisecond))
		SimpleLogger.Printf("Median Tx Landing Time : %s", (time.Duration(median)).Truncate(time.Millisecond))
		SimpleLogger.Printf("P90 Tx Landing Time    : %s", (time.Duration(p90)).Truncate(time.Millisecond))
		SimpleLogger.Printf("P95 Tx Landing Time    : %s", (time.Duration(p95)).Truncate(time.Millisecond))
		SimpleLogger.Printf("P99 Tx Landing Time    : %s", (time.Duration(p99)).Truncate(time.Millisecond))
		SimpleLogger.Printf("")

		DisplayBlocks()
	}
	fmt.Println()
	fmt.Printf("Benchmark results saved to %s\n", LogFileName)
}
