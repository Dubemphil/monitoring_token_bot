// main.go - Solana Token Monitoring Bot for Telegram Calls
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// --- Configuration Structs ---

type Config struct {
	SpreadsheetID       string
	SheetName           string
	HeliusRPCURL        string
	TickIntervalSeconds int
	USDCTokenAddress    string
	SlippageBps         int
}

// --- Data Structs ---

type TokenMonitoring struct {
	RowIndex              int
	TokenAddress          string
	TokenName             string
	Status                string
	MonitorStartPrice     float64 // Price when monitoring started
	CurrentPrice          float64
	MonitorStartMarketCap float64
	CurrentMarketCap      float64
	HighestPrice          float64
	HighestMarketCap      float64
	HighestMultiplier     float64 // Highest multiplier from start price
	CurrentMultiplier     float64 // Current multiplier from start price
	TrailingStopLoss      float64
	StopLossMultiplier    float64
	LastUpdated           time.Time
	// Tracking fields
	ConsecutiveDownticks  int
	LastPriceDirection    string
	StopLossHistory       []StopLossUpdate // Track stop-loss movements
	CallSource            string           // Which telegram channel/source
	CallTime              time.Time        // When the call was made
}

type StopLossUpdate struct {
	Timestamp     time.Time
	OldStopLoss   float64
	NewStopLoss   float64
	OldMultiplier float64
	NewMultiplier float64
	ATHMultiplier float64
	Reason        string
}

// --- Token Metadata Structs ---

type DexScreenerPair struct {
	ChainId     string `json:"chainId"`
	DexId       string `json:"dexId"`
	Url         string `json:"url"`
	PairAddress string `json:"pairAddress"`
	BaseToken   struct {
		Address string `json:"address"`
		Name    string `json:"name"`
		Symbol  string `json:"symbol"`
	} `json:"baseToken"`
	QuoteToken struct {
		Address string `json:"address"`
		Name    string `json:"name"`
		Symbol  string `json:"symbol"`
	} `json:"quoteToken"`
	PriceNative string  `json:"priceNative"`
	PriceUsd    string  `json:"priceUsd"`
	Liquidity   struct {
		Usd   float64 `json:"usd"`
		Base  float64 `json:"base"`
		Quote float64 `json:"quote"`
	} `json:"liquidity"`
	Fdv       float64 `json:"fdv"`
	MarketCap float64 `json:"marketCap"`
}

type DexScreenerResponse struct {
	SchemaVersion string            `json:"schemaVersion"`
	Pairs         []DexScreenerPair `json:"pairs"`
}

// --- Jupiter API Structs ---

type JupiterQuoteResponse struct {
	InputMint            string      `json:"inputMint"`
	InAmount             string      `json:"inAmount"`
	OutputMint           string      `json:"outputMint"`
	OutAmount            string      `json:"outAmount"`
	OtherAmountThreshold string      `json:"otherAmountThreshold"`
	SwapMode             string      `json:"swapMode"`
	SlippageBps          int         `json:"slippageBps"`
	PlatformFee          interface{} `json:"platformFee"`
	PriceImpactPct       string      `json:"priceImpactPct"`
	RoutePlan            []RoutePlan `json:"routePlan"`
	ContextSlot          int64       `json:"contextSlot"`
	TimeTaken            float64     `json:"timeTaken"`
}

type SwapInfo struct {
	AmmKey     string `json:"ammKey"`
	Label      string `json:"label"`
	InputMint  string `json:"inputMint"`
	OutputMint string `json:"outputMint"`
	InAmount   string `json:"inAmount"`
	OutAmount  string `json:"outAmount"`
	FeeAmount  string `json:"feeAmount"`
	FeeMint    string `json:"feeMint"`
}

type RoutePlan struct {
	SwapInfo SwapInfo `json:"swapInfo"`
	Percent  int      `json:"percent"`
}

// Global clients for reuse
var sheetsSvc *sheets.Service
var solanaClient *rpc.Client
var appConfig Config

var jupiterEndpoints = struct {
	QuoteV1 string
}{
	QuoteV1: "https://lite-api.jup.ag/swap/v1/quote",
}

func main() {
	log.Println("🚀 Starting Solana Token Monitoring Bot for Telegram Calls...")

	// 1. Load Configuration
	if err := loadConfig(); err != nil {
		log.Fatalf("❌ Configuration error: %v", err)
	}

	// 2. Test Jupiter API connectivity
	if err := testJupiterAPI(); err != nil {
		log.Fatalf("❌ Jupiter API connectivity error: %v", err)
	}

	// 3. Test network connectivity
	if err := testNetworkConnectivity(); err != nil {
		log.Fatalf("❌ Network connectivity error: %v", err)
	}

	// 4. Initialize Services
	if err := initializeServices(); err != nil {
		log.Fatalf("❌ Service initialization error: %v", err)
	}

	// 5. Create sheet headers if needed
	if err := ensureSheetHeaders(); err != nil {
		log.Printf("⚠️ Warning: Could not ensure sheet headers: %v", err)
	}

	// 6. Start Main Monitoring Loop
	log.Printf("🔄 Starting monitoring loop (%d seconds interval)...", appConfig.TickIntervalSeconds)
	ticker := time.NewTicker(time.Duration(appConfig.TickIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		monitorTokens()
		<-ticker.C
	}
}

func loadConfig() error {
	log.Println("Loading configuration from environment variables...")
	var err error

	getRequiredEnv := func(key string) (string, error) {
		value := os.Getenv(key)
		if value == "" {
			return "", fmt.Errorf("required environment variable %s is not set", key)
		}
		return value, nil
	}

	// Load string values
	appConfig.SpreadsheetID, err = getRequiredEnv("SPREADSHEET_ID")
	if err != nil {
		return err
	}
	appConfig.SheetName, err = getRequiredEnv("SHEET_NAME")
	if err != nil {
		return err
	}
	appConfig.HeliusRPCURL, err = getRequiredEnv("HELIUS_RPC_URL")
	if err != nil {
		return err
	}
	appConfig.USDCTokenAddress, err = getRequiredEnv("USDC_TOKEN_ADDRESS")
	if err != nil {
		return err
	}

	// Load integer values with defaults
	tickIntervalStr := os.Getenv("TICK_INTERVAL_SECONDS")
	if tickIntervalStr == "" {
		appConfig.TickIntervalSeconds = 10 // Default to 10 seconds for monitoring
	} else {
		appConfig.TickIntervalSeconds, err = strconv.Atoi(tickIntervalStr)
		if err != nil {
			return fmt.Errorf("invalid TICK_INTERVAL_SECONDS: %v", err)
		}
	}

	slippageBpsStr := os.Getenv("SLIPPAGE_BPS")
	if slippageBpsStr == "" {
		appConfig.SlippageBps = 100 // Default to 1% slippage for price quotes
	} else {
		appConfig.SlippageBps, err = strconv.Atoi(slippageBpsStr)
		if err != nil {
			return fmt.Errorf("invalid SLIPPAGE_BPS: %v", err)
		}
	}

	log.Printf("✅ Configuration loaded successfully. Tick Interval: %ds", appConfig.TickIntervalSeconds)
	return nil
}

func initializeServices() error {
	ctx := context.Background()
	var err error

	sheetsSvc, err = sheets.NewService(ctx,
		option.WithCredentialsFile("credentials.json"),
		option.WithScopes(sheets.SpreadsheetsScope))
	if err != nil {
		return fmt.Errorf("unable to retrieve Sheets client: %v", err)
	}

	solanaClient = rpc.New(appConfig.HeliusRPCURL)
	log.Println("✅ Services initialized successfully.")
	return nil
}

func testJupiterAPI() error {
	log.Println("🧪 Testing Jupiter API connectivity...")

	solAddress := "So11111111111111111111111111111111111111112"
	testAmount := "100000000" // 0.1 SOL in lamports

	url := fmt.Sprintf("%s?inputMint=%s&outputMint=%s&amount=%s&slippageBps=50&restrictIntermediateTokens=true",
		jupiterEndpoints.QuoteV1,
		solAddress,
		appConfig.USDCTokenAddress,
		testAmount,
	)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("Jupiter API test failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("Jupiter API returned status %d: %s", resp.StatusCode, string(body))
	}

	log.Println("✅ Jupiter API connectivity test passed!")
	return nil
}

func testNetworkConnectivity() error {
	log.Println("🔍 Testing network connectivity...")

	testURLs := []string{
		"https://google.com",
		"https://api.mainnet-beta.solana.com",
		"https://lite-api.jup.ag",
		"https://api.dexscreener.com",
	}

	client := &http.Client{Timeout: 10 * time.Second}

	for _, url := range testURLs {
		resp, err := client.Get(url)
		if err != nil {
			log.Printf("❌ Failed to reach %s: %v", url, err)
			return fmt.Errorf("network connectivity issue: %v", err)
		}
		resp.Body.Close()
		log.Printf("✅ Successfully reached %s", url)
	}

	log.Println("✅ Network connectivity test passed")
	return nil
}

func ensureSheetHeaders() error {
	headers := []interface{}{
		"Token Address", "Token Name", "Status", "Call Source", "Call Time", 
		"Start Price", "Current Price", "Start Market Cap", "Current Market Cap", 
		"Highest Market Cap", "Peak Multiplier", "Current Multiplier", "P/L %", 
		"Trailing Stop-Loss", "Stop-Loss Multiplier", "Stop-Loss Updates", "Log",
	}

	vr := &sheets.ValueRange{Values: [][]interface{}{headers}}
	rangeStr := fmt.Sprintf("%s!A1:Q1", appConfig.SheetName)

	_, err := sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStr, vr).ValueInputOption("RAW").Do()

	return err
}

func getTokenMetadata(tokenAddress string) (string, float64, error) {
	url := fmt.Sprintf("https://api.dexscreener.com/latest/dex/tokens/%s", tokenAddress)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", 0, fmt.Errorf("DexScreener API request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", 0, fmt.Errorf("DexScreener API returned status %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read response: %v", err)
	}

	var response DexScreenerResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", 0, fmt.Errorf("failed to parse response: %v", err)
	}

	if len(response.Pairs) == 0 {
		return "Unknown Token", 0, fmt.Errorf("no pairs found for token")
	}

	pair := response.Pairs[0]
	tokenName := pair.BaseToken.Name
	if tokenName == "" {
		tokenName = pair.BaseToken.Symbol
	}
	if tokenName == "" {
		tokenName = "Unknown Token"
	}

	marketCap := pair.MarketCap
	if marketCap == 0 {
		marketCap = pair.Fdv
	}

	return tokenName, marketCap, nil
}

func monitorTokens() {
	log.Println("--- Monitoring Tokens ---")
	monitoringList, err := readMonitoringListFromSheet()
	if err != nil {
		log.Printf("⚠️ Error reading sheet: %v", err)
		return
	}

	for _, token := range monitoringList {
		switch token.Status {
		case "NEW", "":
			go startMonitoring(token)
		case "MONITORING":
			go monitorActiveToken(token)
		}
	}
}

func startMonitoring(token TokenMonitoring) {
	log.Printf("🆕 Starting to monitor: %s", token.TokenAddress)

	// Get token metadata
	tokenName, startMarketCap, err := getTokenMetadata(token.TokenAddress)
	if err != nil {
		log.Printf("⚠️ Could not get token metadata for %s: %v", token.TokenAddress, err)
		tokenName = "Unknown Token"
		startMarketCap = 0
	}

	token.TokenName = tokenName
	token.MonitorStartMarketCap = startMarketCap

	updateStatusAndLog(token.RowIndex, "INITIALIZING",
		"🔄 Initializing monitoring for %s...", tokenName)

	// Get starting price
	price, err := getJupiterPrice(token.TokenAddress)
	if err != nil {
		log.Printf("⚠️ Could not get starting price for %s: %v", token.TokenAddress, err)
		updateStatusAndLog(token.RowIndex, "FAILED", "❌ Failed to get starting price: %v", err)
		return
	}

	// Initialize monitoring data
	token.MonitorStartPrice = price
	token.CurrentPrice = price
	token.HighestPrice = price
	token.CurrentMarketCap = startMarketCap
	token.HighestMarketCap = startMarketCap
	token.CurrentMultiplier = 1.0
	token.HighestMultiplier = 1.0
	token.TrailingStopLoss = 0 // No initial stop-loss
	token.StopLossMultiplier = 0
	token.ConsecutiveDownticks = 0
	token.LastPriceDirection = "STABLE"
	token.StopLossHistory = []StopLossUpdate{}

	// Update sheet with success
	updateStatusAndLog(token.RowIndex, "MONITORING",
		"✅ MONITORING STARTED! %s | Start Price: $%.8f | Start MC: $%.0f",
		tokenName, price, startMarketCap)

	updateMonitoringData(token)
}

func monitorActiveToken(token TokenMonitoring) {
	log.Printf("👀 Monitoring: %s (%s) - Current: %.2fx", 
		token.TokenName, token.TokenAddress[:8], token.CurrentMultiplier)

	// Get current price
	price, err := getJupiterPrice(token.TokenAddress)
	if err != nil {
		log.Printf("⚠️ Price fetch failed for %s: %v", token.TokenAddress, err)
		return
	}

	// Get current market cap
	_, currentMarketCap, err := getTokenMetadata(token.TokenAddress)
	if err != nil {
		log.Printf("⚠️ Market cap fetch failed for %s: %v", token.TokenAddress, err)
		currentMarketCap = token.CurrentMarketCap // Keep previous value
	}

	// Track price direction
	previousPrice := token.CurrentPrice
	priceDirection := "STABLE"
	if price > previousPrice {
		priceDirection = "UP"
		token.ConsecutiveDownticks = 0
	} else if price < previousPrice {
		priceDirection = "DOWN"
		if token.LastPriceDirection == "DOWN" {
			token.ConsecutiveDownticks++
		} else {
			token.ConsecutiveDownticks = 1
		}
	}
	token.LastPriceDirection = priceDirection

	// Update position
	token.CurrentPrice = price
	token.CurrentMarketCap = currentMarketCap
	token.LastUpdated = time.Now()

	// Calculate current multiplier from start price
	if token.MonitorStartPrice > 0 {
		token.CurrentMultiplier = token.CurrentPrice / token.MonitorStartPrice
	}

	// Update highest price and market cap
	if token.CurrentPrice > token.HighestPrice {
		token.HighestPrice = token.CurrentPrice
		token.HighestMarketCap = currentMarketCap
		token.HighestMultiplier = token.CurrentMultiplier

		// Log ATH with different messages
		if token.HighestMultiplier >= 10.0 {
			updateStatusAndLog(token.RowIndex, "MONITORING",
				"🚀 MAJOR ATH! %s | %.0fx | Price: $%.8f | MC: $%.0f",
				token.TokenName, token.HighestMultiplier, token.CurrentPrice, token.HighestMarketCap)
		} else {
			updateStatusAndLog(token.RowIndex, "MONITORING",
				"🔥 NEW ATH! %s | %.2fx | Price: $%.8f | MC: $%.0f",
				token.TokenName, token.HighestMultiplier, token.CurrentPrice, token.HighestMarketCap)
		}
	}

	// Calculate and update trailing stop-loss
	newStopLoss, newStopMultiplier := calculateTrailingStopLoss(token)

	// Track stop-loss updates
	if newStopLoss > token.TrailingStopLoss && token.TrailingStopLoss > 0 {
		// Stop-loss was raised
		stopLossUpdate := StopLossUpdate{
			Timestamp:     time.Now(),
			OldStopLoss:   token.TrailingStopLoss,
			NewStopLoss:   newStopLoss,
			OldMultiplier: token.StopLossMultiplier,
			NewMultiplier: newStopMultiplier,
			ATHMultiplier: token.HighestMultiplier,
			Reason:        fmt.Sprintf("ATH reached %.2fx", token.HighestMultiplier),
		}
		token.StopLossHistory = append(token.StopLossHistory, stopLossUpdate)

		updateStatusAndLog(token.RowIndex, "MONITORING",
			"📈 TRAILING STOP-LOSS RAISED: %s | %.2fx → %.2fx | ATH: %.2fx",
			token.TokenName, token.StopLossMultiplier, newStopMultiplier, token.HighestMultiplier)

		// Log detailed stop-loss update
		logStopLossUpdate(token.RowIndex, stopLossUpdate)

	} else if newStopLoss > 0 && token.TrailingStopLoss == 0 {
		// Stop-loss activated for first time
		stopLossUpdate := StopLossUpdate{
			Timestamp:     time.Now(),
			OldStopLoss:   0,
			NewStopLoss:   newStopLoss,
			OldMultiplier: 0,
			NewMultiplier: newStopMultiplier,
			ATHMultiplier: token.HighestMultiplier,
			Reason:        "Stop-loss activated at 2x",
		}
		token.StopLossHistory = append(token.StopLossHistory, stopLossUpdate)

		updateStatusAndLog(token.RowIndex, "MONITORING",
			"✅ TRAILING STOP-LOSS ACTIVATED! %s | Protected at %.2fx | ATH: %.2fx",
			token.TokenName, newStopMultiplier, token.HighestMultiplier)

		logStopLossUpdate(token.RowIndex, stopLossUpdate)
	}

	token.TrailingStopLoss = newStopLoss
	token.StopLossMultiplier = newStopMultiplier

	// Check if stop-loss would be triggered (for logging purposes)
	if token.TrailingStopLoss > 0 && token.CurrentPrice <= token.TrailingStopLoss {
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"🛑 STOP-LOSS TRIGGERED! %s | Current: %.2fx | Stop: %.2fx | Peak was: %.2fx",
			token.TokenName, token.CurrentMultiplier, token.StopLossMultiplier, token.HighestMultiplier)
	}

	// Update sheet with current data
	updateMonitoringData(token)
}

func calculateTrailingStopLoss(token TokenMonitoring) (float64, float64) {
	if token.MonitorStartPrice <= 0 {
		return 0, 0
	}

	highestMultiplier := token.HighestPrice / token.MonitorStartPrice

	// No stop-loss until 2x is reached
	if highestMultiplier < 2.0 {
		return 0, 0
	}

	// Progressive stop-loss logic
	var baseStopLossMultiplier float64

	switch {
	case highestMultiplier >= 500:
		baseStopLossMultiplier = highestMultiplier * 0.88
	case highestMultiplier >= 100:
		baseStopLossMultiplier = highestMultiplier * 0.87
	case highestMultiplier >= 50:
		baseStopLossMultiplier = highestMultiplier * 0.86
	case highestMultiplier >= 20:
		baseStopLossMultiplier = highestMultiplier * 0.85
	case highestMultiplier >= 10:
		baseStopLossMultiplier = highestMultiplier * 0.83
	case highestMultiplier >= 4:
		baseStopLossMultiplier = highestMultiplier * 0.80
	case highestMultiplier >= 2:
		baseStopLossMultiplier = math.Max(highestMultiplier*0.78, 1.3)
	default:
		return 0, 0
	}

	// Volatility adjustment
	adjustedStopLossMultiplier := baseStopLossMultiplier
	if token.ConsecutiveDownticks >= 3 {
		volatilityAdjustment := 0.95
		adjustedStopLossMultiplier = baseStopLossMultiplier * volatilityAdjustment
	}

	// Ensure minimum profitable levels
	switch {
	case highestMultiplier >= 10:
		adjustedStopLossMultiplier = math.Max(adjustedStopLossMultiplier, 5.0)
	case highestMultiplier >= 5:
		adjustedStopLossMultiplier = math.Max(adjustedStopLossMultiplier, 2.5)
	case highestMultiplier >= 2:
		adjustedStopLossMultiplier = math.Max(adjustedStopLossMultiplier, 1.2)
	}

	stopLossPrice := token.MonitorStartPrice * adjustedStopLossMultiplier
	return stopLossPrice, adjustedStopLossMultiplier
}

func getJupiterPrice(tokenAddress string) (float64, error) {
	price, err := getPriceFromQuote(tokenAddress)
	if err == nil && price > 0 {
		return price, nil
	}
	log.Printf("⚠️ Quote API failed for %s: %v", tokenAddress, err)

	return getPriceFromAlternativeEndpoints(tokenAddress)
}

func getPriceFromQuote(tokenAddress string) (float64, error) {
	testAmounts := []string{"1000000", "100000", "10000", "1000"}

	for i, testAmount := range testAmounts {
		url := fmt.Sprintf("%s?inputMint=%s&outputMint=%s&amount=%s&slippageBps=%d&restrictIntermediateTokens=true",
			jupiterEndpoints.QuoteV1,
			tokenAddress,
			appConfig.USDCTokenAddress,
			testAmount,
			appConfig.SlippageBps,
		)

		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			if i == len(testAmounts)-1 {
				return 0, fmt.Errorf("quote request failed: %v", err)
			}
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			body, _ := ioutil.ReadAll(resp.Body)
			if i == len(testAmounts)-1 {
				return 0, fmt.Errorf("quote failed with status %d: %s", resp.StatusCode, string(body))
			}
			continue
		}

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			if i == len(testAmounts)-1 {
				return 0, fmt.Errorf("failed to read response: %v", err)
			}
			continue
		}

		var quote JupiterQuoteResponse
		if err := json.Unmarshal(body, &quote); err != nil {
			if i == len(testAmounts)-1 {
				return 0, fmt.Errorf("failed to parse quote: %v", err)
			}
			continue
		}

		inAmount, err := strconv.ParseFloat(quote.InAmount, 64)
		if err != nil {
			continue
		}

		outAmount, err := strconv.ParseFloat(quote.OutAmount, 64)
		if err != nil {
			continue
		}

		if inAmount == 0 {
			continue
		}

		price := outAmount / inAmount

		if price < 0.000001 && price > 0 {
			price = price * 1000000
		}

		if price > 0 {
			return price, nil
		}
	}

	return 0, fmt.Errorf("could not get valid price with any test amount")
}

func getPriceFromAlternativeEndpoints(tokenAddress string) (float64, error) {
	testUSDCAmount := "1000000" // 1 USDC

	url := fmt.Sprintf("%s?inputMint=%s&outputMint=%s&amount=%s&slippageBps=%d&restrictIntermediateTokens=true",
		jupiterEndpoints.QuoteV1,
		appConfig.USDCTokenAddress,
		tokenAddress,
		testUSDCAmount,
		appConfig.SlippageBps,
	)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("reverse quote failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return 0, fmt.Errorf("reverse quote failed with status %d: %s", resp.StatusCode, string(body))
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response: %v", err)
	}

	var quote JupiterQuoteResponse
	if err := json.Unmarshal(body, &quote); err != nil {
		return 0, fmt.Errorf("failed to parse reverse quote: %v", err)
	}

	inAmount, err := strconv.ParseFloat(quote.InAmount, 64)  // USDC input
	if err != nil {
		return 0, fmt.Errorf("invalid inAmount in reverse quote: %v", err)
	}

	outAmount, err := strconv.ParseFloat(quote.OutAmount, 64) // Token output
	if err != nil {
		return 0, fmt.Errorf("invalid outAmount in reverse quote: %v", err)
	}

	if outAmount == 0 {
		return 0, fmt.Errorf("invalid token output amount")
	}

	price := inAmount / outAmount
	return price, nil
}

// Sheet Functions

func readMonitoringListFromSheet() ([]TokenMonitoring, error) {
	readRange := fmt.Sprintf("%s!A2:Q", appConfig.SheetName)
	resp, err := sheetsSvc.Spreadsheets.Values.Get(appConfig.SpreadsheetID, readRange).Do()
	if err != nil {
		return nil, err
	}

	var tokens []TokenMonitoring
	if len(resp.Values) == 0 {
		return tokens, nil
	}

	for i, row := range resp.Values {
		if len(row) == 0 || row[0] == "" {
			continue
		}

		token := TokenMonitoring{
			RowIndex:     i + 2,
			TokenAddress: fmt.Sprintf("%v", row[0]),
		}

		if len(row) > 1 { token.TokenName = fmt.Sprintf("%v", row[1]) }
		if len(row) > 2 { token.Status = fmt.Sprintf("%v", row[2]) }
		if len(row) > 3 { token.CallSource = fmt.Sprintf("%v", row[3]) }
		// Parse call time if present
		if len(row) > 4 {
			if timeStr := fmt.Sprintf("%v", row[4]); timeStr != "" {
				if callTime, err := time.Parse("2006-01-02 15:04:05", timeStr); err == nil {
					token.CallTime = callTime
				}
			}
		}
		if len(row) > 5 { token.MonitorStartPrice, _ = strconv.ParseFloat(fmt.Sprintf("%v", row[5]), 64) }
		if len(row) > 6 { token.CurrentPrice, _ = strconv.ParseFloat(fmt.Sprintf("%v", row[6]), 64) }
		if len(row) > 7 { token.MonitorStartMarketCap, _ = strconv.ParseFloat(fmt.Sprintf("%v", row[7]), 64) }
		if len(row) > 8 { token.CurrentMarketCap, _ = strconv.ParseFloat(fmt.Sprintf("%v", row[8]), 64) }
		if len(row) > 9 { token.HighestMarketCap, _ = strconv.ParseFloat(fmt.Sprintf("%v", row[9]), 64) }
		if len(row) > 10 { 
			peakStr := fmt.Sprintf("%v", row[10])
			if strings.HasSuffix(peakStr, "x") {
				peakStr = strings.TrimSuffix(peakStr, "x")
			}
			token.HighestMultiplier, _ = strconv.ParseFloat(peakStr, 64) 
		}
		if len(row) > 11 { 
			currentStr := fmt.Sprintf("%v", row[11])
			if strings.HasSuffix(currentStr, "x") {
				currentStr = strings.TrimSuffix(currentStr, "x")
			}
			token.CurrentMultiplier, _ = strconv.ParseFloat(currentStr, 64) 
		}
		if len(row) > 13 { token.TrailingStopLoss, _ = strconv.ParseFloat(fmt.Sprintf("%v", row[13]), 64) }
		if len(row) > 14 { 
			stopStr := fmt.Sprintf("%v", row[14])
			if strings.HasSuffix(stopStr, "x") {
				stopStr = strings.TrimSuffix(stopStr, "x")
			}
			token.StopLossMultiplier, _ = strconv.ParseFloat(stopStr, 64) 
		}

		if token.Status == "" {
			token.Status = "NEW"
		}

		// Initialize tracking fields
		token.ConsecutiveDownticks = 0
		token.LastPriceDirection = "STABLE"
		token.StopLossHistory = []StopLossUpdate{}

		tokens = append(tokens, token)
	}
	return tokens, nil
}

func updateMonitoringData(token TokenMonitoring) {
	profitPercent := 0.0
	if token.MonitorStartPrice > 0 {
		profitPercent = ((token.CurrentPrice / token.MonitorStartPrice) - 1) * 100
	}

	callTimeStr := ""
	if !token.CallTime.IsZero() {
		callTimeStr = token.CallTime.Format("2006-01-02 15:04:05")
	}

	vr := &sheets.ValueRange{
		Values: [][]interface{}{
			{
				token.TokenName,
				token.CallSource,
				callTimeStr,
				token.MonitorStartPrice,
				token.CurrentPrice,
				token.MonitorStartMarketCap,
				token.CurrentMarketCap,
				token.HighestMarketCap,
				fmt.Sprintf("%.2fx", token.HighestMultiplier),
				fmt.Sprintf("%.2fx", token.CurrentMultiplier),
				fmt.Sprintf("%.1f%%", profitPercent),
				token.TrailingStopLoss,
				fmt.Sprintf("%.2fx", token.StopLossMultiplier),
			},
		},
	}

	rangeStr := fmt.Sprintf("%s!B%d:N%d", appConfig.SheetName, token.RowIndex, token.RowIndex)
	_, err := sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStr, vr).ValueInputOption("USER_ENTERED").Do()

	if err != nil {
		log.Printf("⚠️ Failed to update monitoring data: %v", err)
	}
}

func updateStatusAndLog(rowIndex int, status string, logMessage string, args ...interface{}) {
	// Update Status (column C)
	vrStatus := &sheets.ValueRange{Values: [][]interface{}{{status}}}
	rangeStrStatus := fmt.Sprintf("%s!C%d", appConfig.SheetName, rowIndex)
	_, err := sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStrStatus, vrStatus).ValueInputOption("RAW").Do()
	if err != nil {
		log.Printf("⚠️ Failed to update status: %v", err)
	}

	// Append to Log (column Q)
	fullLogMessage := fmt.Sprintf(logMessage, args...)
	timestampedLog := fmt.Sprintf("[%s] %s\n",
		time.Now().Format("2006-01-02 15:04:05"), fullLogMessage)

	// Get existing log
	rangeStrLogGet := fmt.Sprintf("%s!Q%d", appConfig.SheetName, rowIndex)
	resp, err := sheetsSvc.Spreadsheets.Values.Get(appConfig.SpreadsheetID, rangeStrLogGet).Do()
	if err != nil {
		log.Printf("⚠️ Failed to read existing log: %v", err)
		return
	}

	existingLog := ""
	if len(resp.Values) > 0 && len(resp.Values[0]) > 0 {
		existingLog = fmt.Sprintf("%v", resp.Values[0][0])
	}

	newLog := existingLog + timestampedLog
	vrLog := &sheets.ValueRange{Values: [][]interface{}{{newLog}}}
	_, err = sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStrLogGet, vrLog).ValueInputOption("RAW").Do()
	if err != nil {
		log.Printf("⚠️ Failed to append log: %v", err)
	}

	// Also log to console
	log.Printf("📝 Row %d: %s", rowIndex, fullLogMessage)
}

func logStopLossUpdate(rowIndex int, update StopLossUpdate) {
	// Log stop-loss update to dedicated column (column P)
	stopLossLogMessage := fmt.Sprintf("[%s] Stop-Loss: %.2fx → %.2fx (ATH: %.2fx) - %s\n",
		update.Timestamp.Format("2006-01-02 15:04:05"),
		update.OldMultiplier,
		update.NewMultiplier,
		update.ATHMultiplier,
		update.Reason)

	// Get existing stop-loss log
	rangeStrStopLossGet := fmt.Sprintf("%s!P%d", appConfig.SheetName, rowIndex)
	resp, err := sheetsSvc.Spreadsheets.Values.Get(appConfig.SpreadsheetID, rangeStrStopLossGet).Do()
	if err != nil {
		log.Printf("⚠️ Failed to read existing stop-loss log: %v", err)
		return
	}

	existingStopLossLog := ""
	if len(resp.Values) > 0 && len(resp.Values[0]) > 0 {
		existingStopLossLog = fmt.Sprintf("%v", resp.Values[0][0])
	}

	newStopLossLog := existingStopLossLog + stopLossLogMessage
	vrStopLossLog := &sheets.ValueRange{Values: [][]interface{}{{newStopLossLog}}}
	_, err = sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStrStopLossGet, vrStopLossLog).ValueInputOption("RAW").Do()
	if err != nil {
		log.Printf("⚠️ Failed to append stop-loss log: %v", err)
	}

	// Also log to console with special formatting.
	log.Printf("🛡️ STOP-LOSS UPDATE Row %d: %.2fx → %.2fx (ATH: %.2fx) - %s", 
		rowIndex, update.OldMultiplier, update.NewMultiplier, update.ATHMultiplier, update.Reason)
}