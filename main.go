// main.go - Solana Token Monitoring Bot for Telegram Calls.
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

	// Update status only, no logging
	vrStatus := &sheets.ValueRange{Values: [][]interface{}{{"INITIALIZING"}}}
	rangeStrStatus := fmt.Sprintf("%s!C%d", appConfig.SheetName, token.RowIndex)
	sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStrStatus, vrStatus).ValueInputOption("RAW").Do()

	// Get starting price
	price, err := getJupiterPrice(token.TokenAddress)
	if err != nil {
		log.Printf("⚠️ Could not get starting price for %s: %v", token.TokenAddress, err)
		vrStatus = &sheets.ValueRange{Values: [][]interface{}{{"FAILED"}}}
		sheetsSvc.Spreadsheets.Values.Update(
			appConfig.SpreadsheetID, rangeStrStatus, vrStatus).ValueInputOption("RAW").Do()
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

	// Update status to MONITORING - no verbose logging
	vrStatus = &sheets.ValueRange{Values: [][]interface{}{{"MONITORING"}}}
	sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStrStatus, vrStatus).ValueInputOption("RAW").Do()

	updateMonitoringData(token)
	log.Printf("✅ Started monitoring %s at $%.8f", tokenName, price)
}

// Enhanced monitoring functions with comprehensive logging

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
	previousMultiplier := token.CurrentMultiplier
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
	isNewATH := false
	if token.CurrentPrice > token.HighestPrice {
		token.HighestPrice = token.CurrentPrice
		token.HighestMarketCap = currentMarketCap
		token.HighestMultiplier = token.CurrentMultiplier
		isNewATH = true

		// Log ATH achievements with different thresholds
		logATHAchievement(token)
	}

	// Calculate and update trailing stop-loss
	oldStopLoss := token.TrailingStopLoss
	oldStopMultiplier := token.StopLossMultiplier
	newStopLoss, newStopMultiplier := calculateTrailingStopLoss(token)

	// Log multiplier milestones (both up and down movements)
	logMultiplierMilestones(token, previousMultiplier, token.CurrentMultiplier)

	// Track stop-loss updates with enhanced logging
	if newStopLoss != oldStopLoss || newStopMultiplier != oldStopMultiplier {
		handleStopLossUpdate(token, oldStopLoss, newStopLoss, oldStopMultiplier, newStopMultiplier, isNewATH)
	}

	token.TrailingStopLoss = newStopLoss
	token.StopLossMultiplier = newStopMultiplier

	// Enhanced stop-loss trigger logging
	if token.TrailingStopLoss > 0 && token.CurrentPrice <= token.TrailingStopLoss {
		percentDrop := ((token.HighestMultiplier - token.CurrentMultiplier) / token.HighestMultiplier) * 100
		updateStatusAndLog(token.RowIndex, "STOP-LOSS TRIGGERED",
			"🛑 STOP-LOSS TRIGGERED! %s | Current: %.2fx | Stop: %.2fx | Peak was: %.2fx | Dropped %.1f%% from peak | MC: $%.0f",
			token.TokenName, token.CurrentMultiplier, token.StopLossMultiplier, token.HighestMultiplier, percentDrop, token.CurrentMarketCap)
	}

	// Update sheet with current data
	updateMonitoringData(token)
}

// Log ATH achievements with different messaging based on multiplier levels
func logATHAchievement(token TokenMonitoring) {
	multiplier := token.HighestMultiplier
	
	switch {
	case multiplier >= 100.0:
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"🚀🚀🚀 LEGENDARY ATH! %s | %.1fx | Price: $%.8f | MC: $%.0f | Protected at: %.1fx (%.0f%% secured)",
			token.TokenName, multiplier, token.CurrentPrice, token.HighestMarketCap, 
			multiplier*0.87, (multiplier*0.87-1)*100)
	case multiplier >= 50.0:
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"🚀🚀 MASSIVE ATH! %s | %.1fx | Price: $%.8f | MC: $%.0f | Protected at: %.1fx (%.0f%% secured)",
			token.TokenName, multiplier, token.CurrentPrice, token.HighestMarketCap, 
			multiplier*0.86, (multiplier*0.86-1)*100)
	case multiplier >= 20.0:
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"🚀 HUGE ATH! %s | %.1fx | Price: $%.8f | MC: $%.0f | Protected at: %.1fx (%.0f%% secured)",
			token.TokenName, multiplier, token.CurrentPrice, token.HighestMarketCap, 
			multiplier*0.85, (multiplier*0.85-1)*100)
	case multiplier >= 10.0:
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"🔥 MAJOR ATH! %s | %.1fx | Price: $%.8f | MC: $%.0f | Protected at: %.1fx (%.0f%% secured)",
			token.TokenName, multiplier, token.CurrentPrice, token.HighestMarketCap, 
			multiplier*0.83, (multiplier*0.83-1)*100)
	case multiplier >= 4.0:
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"📈 STRONG ATH! %s | %.2fx | Price: $%.8f | MC: $%.0f | Protected at: %.2fx (%.0f%% secured)",
			token.TokenName, multiplier, token.CurrentPrice, token.HighestMarketCap, 
			multiplier*0.80, (multiplier*0.80-1)*100)
	case multiplier >= 2.0:
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"✅ NEW ATH! %s | %.2fx | Price: $%.8f | MC: $%.0f | Protected at: %.2fx (%.0f%% secured)",
			token.TokenName, multiplier, token.CurrentPrice, token.HighestMarketCap, 
			math.Max(multiplier*0.78, 1.3), (math.Max(multiplier*0.78, 1.3)-1)*100)
	case multiplier >= 1.5:
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"📊 Moving Up! %s | %.2fx | Price: $%.8f | MC: $%.0f | No protection yet (need 2x)",
			token.TokenName, multiplier, token.CurrentPrice, token.HighestMarketCap)
	}
}

// Log all multiplier milestones (both increases and decreases)
func logMultiplierMilestones(token TokenMonitoring, previousMultiplier, currentMultiplier float64) {
	// Define significant milestone thresholds
	milestones := []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0, 1.2, 1.5, 1.8, 2.0, 2.5, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0, 10.0, 15.0, 20.0, 25.0, 30.0, 40.0, 50.0, 75.0, 100.0, 150.0, 200.0, 300.0, 500.0, 1000.0}
	
	// Check for crossing milestones in either direction
	for _, milestone := range milestones {
		// Crossing upward
		if previousMultiplier < milestone && currentMultiplier >= milestone {
			logMilestoneEvent(token, milestone, "UP")
		}
		// Crossing downward
		if previousMultiplier > milestone && currentMultiplier <= milestone {
			logMilestoneEvent(token, milestone, "DOWN")
		}
	}
}

// Log specific milestone events
func logMilestoneEvent(token TokenMonitoring, milestone float64, direction string) {
	var emoji, message string
	
	if direction == "UP" {
		switch {
		case milestone >= 100:
			emoji = "🚀🚀🚀"
			message = "LEGENDARY MILESTONE"
		case milestone >= 50:
			emoji = "🚀🚀"
			message = "MASSIVE MILESTONE"
		case milestone >= 10:
			emoji = "🚀"
			message = "MAJOR MILESTONE"
		case milestone >= 2:
			emoji = "🔥"
			message = "STRONG MILESTONE"
		case milestone >= 1:
			emoji = "✅"
			message = "BREAKEVEN+"
		case milestone >= 0.5:
			emoji = "📈"
			message = "RECOVERY"
		default:
			emoji = "📊"
			message = "MILESTONE"
		}
		
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"%s %s REACHED! %s | %.2fx | Price: $%.8f | MC: $%.0f | Peak: %.2fx",
			emoji, message, token.TokenName, milestone, token.CurrentPrice, token.CurrentMarketCap, token.HighestMultiplier)
	} else {
		switch {
		case milestone >= 10:
			emoji = "📉"
			message = "MAJOR PULLBACK"
		case milestone >= 2:
			emoji = "⚠️"
			message = "PULLBACK"
		case milestone >= 1:
			emoji = "🔴"
			message = "BELOW BREAKEVEN"
		case milestone >= 0.5:
			emoji = "💔"
			message = "MAJOR LOSS"
		default:
			emoji = "📉"
			message = "DECLINE"
		}
		
		percentFromPeak := ((token.HighestMultiplier - milestone) / token.HighestMultiplier) * 100
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"%s %s: %s | %.2fx | Price: $%.8f | MC: $%.0f | Down %.1f%% from peak %.2fx",
			emoji, message, token.TokenName, milestone, token.CurrentPrice, token.CurrentMarketCap, percentFromPeak, token.HighestMultiplier)
	}
}

// Handle stop-loss updates with comprehensive logging
func handleStopLossUpdate(token TokenMonitoring, oldStopLoss, newStopLoss, oldStopMultiplier, newStopMultiplier float64, isNewATH bool) {
	var reason string
	
	if isNewATH {
		reason = fmt.Sprintf("ATH reached %.2fx", token.HighestMultiplier)
	} else if newStopLoss > oldStopLoss {
		reason = "Stop-loss raised due to price movement"
	} else if newStopLoss < oldStopLoss {
		reason = "Stop-loss lowered due to price decline"
	} else if oldStopLoss == 0 && newStopLoss > 0 {
		reason = "Stop-loss activated at 2x"
	} else {
		reason = "Stop-loss recalculated"
	}

	// Create stop-loss update record
	stopLossUpdate := StopLossUpdate{
		Timestamp:     time.Now(),
		OldStopLoss:   oldStopLoss,
		NewStopLoss:   newStopLoss,
		OldMultiplier: oldStopMultiplier,
		NewMultiplier: newStopMultiplier,
		ATHMultiplier: token.HighestMultiplier,
		Reason:        reason,
	}
	token.StopLossHistory = append(token.StopLossHistory, stopLossUpdate)

	// Log different types of stop-loss events
	if oldStopLoss == 0 && newStopLoss > 0 {
		// Stop-loss activated for first time
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"✅ TRAILING STOP-LOSS ACTIVATED! %s | Protected at %.2fx | Peak: %.2fx | Securing +%.0f%% profit | MC: $%.0f",
			token.TokenName, newStopMultiplier, token.HighestMultiplier, (newStopMultiplier-1)*100, token.CurrentMarketCap)
	} else if newStopLoss > oldStopLoss {
		// Stop-loss was raised
		profitIncrease := (newStopMultiplier - oldStopMultiplier) * 100
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"📈 STOP-LOSS RAISED: %s | %.2fx → %.2fx | Peak: %.2fx | Profit secured +%.1f%% more | MC: $%.0f",
			token.TokenName, oldStopMultiplier, newStopMultiplier, token.HighestMultiplier, profitIncrease, token.CurrentMarketCap)
	} else if newStopLoss < oldStopLoss && newStopLoss > 0 {
		// Stop-loss was lowered (shouldn't happen normally, but good to track)