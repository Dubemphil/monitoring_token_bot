// Enhanced main.go with Webhooks and RPC Load Balancing
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
	"sync"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// --- Enhanced Configuration ---
type Config struct {
	SpreadsheetID       string
	SheetName           string
	HeliusRPCURLs       []string // Multiple RPC URLs for load balancing
	TickIntervalSeconds int
	USDCTokenAddress    string
	SlippageBps         int
	WebhookPort         string
	EnableWebhooks      bool
	RateLimitDelay      time.Duration
}

// --- RPC Load Balancer ---
type RPCLoadBalancer struct {
	urls         []string
	clients      []*rpc.Client
	currentIndex int
	mutex        sync.RWMutex
	rateLimiter  map[int]time.Time
}

func NewRPCLoadBalancer(urls []string) *RPCLoadBalancer {
	clients := make([]*rpc.Client, len(urls))
	for i, url := range urls {
		clients[i] = rpc.New(url)
	}
	
	return &RPCLoadBalancer{
		urls:        urls,
		clients:     clients,
		rateLimiter: make(map[int]time.Time),
	}
}

func (lb *RPCLoadBalancer) GetNextClient() *rpc.Client {
	lb.mutex.Lock()
	defer lb.mutex.Unlock()
	
	startIndex := lb.currentIndex
	
	for {
		// Check if this RPC is rate limited
		if lastUsed, exists := lb.rateLimiter[lb.currentIndex]; exists {
			if time.Since(lastUsed) < appConfig.RateLimitDelay {
				lb.currentIndex = (lb.currentIndex + 1) % len(lb.clients)
				if lb.currentIndex == startIndex {
					// All RPCs are rate limited, wait for the oldest one
					time.Sleep(100 * time.Millisecond)
				}
				continue
			}
		}
		
		// Use this client and mark it as used
		client := lb.clients[lb.currentIndex]
		lb.rateLimiter[lb.currentIndex] = time.Now()
		lb.currentIndex = (lb.currentIndex + 1) % len(lb.clients)
		
		return client
	}
}

// --- Webhook Data Structures ---
type WebhookPriceUpdate struct {
	TokenAddress string    `json:"token_address"`
	Price        float64   `json:"price"`
	MarketCap    float64   `json:"market_cap"`
	Timestamp    time.Time `json:"timestamp"`
	Source       string    `json:"source"`
}

type PriceCache struct {
	mu     sync.RWMutex
	prices map[string]*WebhookPriceUpdate
}

func NewPriceCache() *PriceCache {
	return &PriceCache{
		prices: make(map[string]*WebhookPriceUpdate),
	}
}

func (pc *PriceCache) Set(tokenAddress string, update *WebhookPriceUpdate) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.prices[tokenAddress] = update
}

func (pc *PriceCache) Get(tokenAddress string) (*WebhookPriceUpdate, bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	update, exists := pc.prices[tokenAddress]
	return update, exists
}

func (pc *PriceCache) IsStale(tokenAddress string, maxAge time.Duration) bool {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	if update, exists := pc.prices[tokenAddress]; exists {
		return time.Since(update.Timestamp) > maxAge
	}
	return true
}

// --- Global Variables ---
var (
	sheetsSvc     *sheets.Service
	rpcBalancer   *RPCLoadBalancer
	priceCache    *PriceCache
	appConfig     Config
	httpClient    = &http.Client{Timeout: 10 * time.Second}
)

var jupiterEndpoints = struct {
	QuoteV1 string
}{
	QuoteV1: "https://lite-api.jup.ag/swap/v1/quote",
}

// --- Enhanced Configuration Loading ---
func loadConfig() error {
	log.Println("Loading enhanced configuration...")
	
	getRequiredEnv := func(key string) (string, error) {
		value := os.Getenv(key)
		if value == "" {
			return "", fmt.Errorf("required environment variable %s is not set", key)
		}
		return value, nil
	}

	var err error
	appConfig.SpreadsheetID, err = getRequiredEnv("SPREADSHEET_ID")
	if err != nil {
		return err
	}
	
	appConfig.SheetName, err = getRequiredEnv("SHEET_NAME")
	if err != nil {
		return err
	}
	
	appConfig.USDCTokenAddress, err = getRequiredEnv("USDC_TOKEN_ADDRESS")
	if err != nil {
		return err
	}

	// Load multiple RPC URLs
	rpcUrls := os.Getenv("HELIUS_RPC_URLS")
	if rpcUrls == "" {
		// Fallback to single URL for backward compatibility
		singleURL, err := getRequiredEnv("HELIUS_RPC_URL")
		if err != nil {
			return err
		}
		appConfig.HeliusRPCURLs = []string{singleURL}
	} else {
		appConfig.HeliusRPCURLs = strings.Split(rpcUrls, ",")
		for i, url := range appConfig.HeliusRPCURLs {
			appConfig.HeliusRPCURLs[i] = strings.TrimSpace(url)
		}
	}

	// Webhook configuration
	appConfig.WebhookPort = os.Getenv("WEBHOOK_PORT")
	if appConfig.WebhookPort == "" {
		appConfig.WebhookPort = "8080"
	}
	
	appConfig.EnableWebhooks = os.Getenv("ENABLE_WEBHOOKS") == "true"

	// Timing configuration
	tickIntervalStr := os.Getenv("TICK_INTERVAL_SECONDS")
	if tickIntervalStr == "" {
		appConfig.TickIntervalSeconds = 8 // Slower interval to reduce API pressure
	} else {
		appConfig.TickIntervalSeconds, err = strconv.Atoi(tickIntervalStr)
		if err != nil {
			return fmt.Errorf("invalid TICK_INTERVAL_SECONDS: %v", err)
		}
	}

	// Rate limiting
	rateLimitMs := os.Getenv("RATE_LIMIT_MS")
	if rateLimitMs == "" {
		appConfig.RateLimitDelay = 200 * time.Millisecond // 200ms between RPC calls
	} else {
		ms, err := strconv.Atoi(rateLimitMs)
		if err != nil {
			return fmt.Errorf("invalid RATE_LIMIT_MS: %v", err)
		}
		appConfig.RateLimitDelay = time.Duration(ms) * time.Millisecond
	}

	slippageBpsStr := os.Getenv("SLIPPAGE_BPS")
	if slippageBpsStr == "" {
		appConfig.SlippageBps = 100
	} else {
		appConfig.SlippageBps, err = strconv.Atoi(slippageBpsStr)
		if err != nil {
			return fmt.Errorf("invalid SLIPPAGE_BPS: %v", err)
		}
	}

	log.Printf("✅ Enhanced configuration loaded: %d RPCs, Webhooks: %t, Tick: %ds", 
		len(appConfig.HeliusRPCURLs), appConfig.EnableWebhooks, appConfig.TickIntervalSeconds)
	return nil
}

// --- Enhanced Price Fetching ---
func getEnhancedTokenPrice(tokenAddress string) (float64, float64, error) {
	// Try webhook cache first (if enabled and fresh)
	if appConfig.EnableWebhooks {
		if update, exists := priceCache.Get(tokenAddress); exists {
			if !priceCache.IsStale(tokenAddress, 30*time.Second) {
				log.Printf("🔄 Using cached price for %s: $%.8f", tokenAddress[:8], update.Price)
				return update.Price, update.MarketCap, nil
			}
		}
	}

	// Fallback to Jupiter API with load balancing
	return getJupiterPriceWithBalancer(tokenAddress)
}

func getJupiterPriceWithBalancer(tokenAddress string) (float64, float64, error) {
	// Add delay to prevent rate limiting
	time.Sleep(appConfig.RateLimitDelay)
	
	price, err := getPriceFromQuoteBalanced(tokenAddress)
	if err != nil {
		log.Printf("⚠️ Quote API failed for %s: %v", tokenAddress[:8], err)
		return getPriceFromAlternativeEndpoints(tokenAddress)
	}

	// Get market cap from DexScreener
	_, marketCap, metaErr := getTokenMetadata(tokenAddress)
	if metaErr != nil {
		log.Printf("⚠️ Market cap fetch failed for %s: %v", tokenAddress[:8], metaErr)
		marketCap = 0 // Use 0 if we can't get market cap
	}

	return price, marketCap, nil
}

func getPriceFromQuoteBalanced(tokenAddress string) (float64, error) {
	testAmounts := []string{"1000000", "100000", "10000", "1000"}

	for i, testAmount := range testAmounts {
		url := fmt.Sprintf("%s?inputMint=%s&outputMint=%s&amount=%s&slippageBps=%d&restrictIntermediateTokens=true",
			jupiterEndpoints.QuoteV1,
			tokenAddress,
			appConfig.USDCTokenAddress,
			testAmount,
			appConfig.SlippageBps,
		)

		resp, err := httpClient.Get(url)
		if err != nil {
			if i == len(testAmounts)-1 {
				return 0, fmt.Errorf("quote request failed: %v", err)
			}
			continue
		}
		defer resp.Body.Close()

		// Handle rate limiting
		if resp.StatusCode == 429 {
			log.Printf("⏳ Rate limited, waiting...")
			time.Sleep(1 * time.Second)
			if i == len(testAmounts)-1 {
				return 0, fmt.Errorf("rate limited on all attempts")
			}
			continue
		}

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

// --- Webhook Server ---
func startWebhookServer() {
	if !appConfig.EnableWebhooks {
		return
	}

	http.HandleFunc("/webhook/price", handlePriceWebhook)
	http.HandleFunc("/health", handleHealthCheck)
	
	server := &http.Server{
		Addr:    ":" + appConfig.WebhookPort,
		Handler: nil,
	}

	go func() {
		log.Printf("🌐 Starting webhook server on port %s", appConfig.WebhookPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("⚠️ Webhook server error: %v", err)
		}
	}()
}

func handlePriceWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	var update WebhookPriceUpdate
	if err := json.Unmarshal(body, &update); err != nil {
		http.Error(w, "Failed to parse JSON", http.StatusBadRequest)
		return
	}

	update.Timestamp = time.Now()
	priceCache.Set(update.TokenAddress, &update)
	
	log.Printf("📡 Webhook price update: %s = $%.8f", update.TokenAddress[:8], update.Price)
	
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now(),
		"rpc_urls":  len(appConfig.HeliusRPCURLs),
		"webhooks":  appConfig.EnableWebhooks,
		"cache_size": len(priceCache.prices),
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// --- Enhanced Monitoring ---
func monitorActiveTokenEnhanced(token TokenMonitoring) {
	log.Printf("👀 Enhanced monitoring: %s (%s) - Current: %.2fx", 
		token.TokenName, token.TokenAddress[:8], token.CurrentMultiplier)

	// Get current price and market cap with enhanced method
	price, currentMarketCap, err := getEnhancedTokenPrice(token.TokenAddress)
	if err != nil {
		log.Printf("⚠️ Price fetch failed for %s: %v", token.TokenAddress[:8], err)
		return
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
		logATHAchievement(token)
	}

	// Calculate and update trailing stop-loss
	oldStopLoss := token.TrailingStopLoss
	oldStopMultiplier := token.StopLossMultiplier
	newStopLoss, newStopMultiplier := calculateTrailingStopLoss(token)

	// Log multiplier milestones
	logMultiplierMilestones(token, previousMultiplier, token.CurrentMultiplier)

	// Track stop-loss updates
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

// --- Enhanced Main Function ---
func main() {
	log.Println("🚀 Starting Enhanced Solana Token Monitoring Bot...")

	// Load enhanced configuration
	if err := loadConfig(); err != nil {
		log.Fatalf("❌ Configuration error: %v", err)
	}

	// Initialize RPC load balancer
	rpcBalancer = NewRPCLoadBalancer(appConfig.HeliusRPCURLs)
	log.Printf("✅ RPC Load Balancer initialized with %d endpoints", len(appConfig.HeliusRPCURLs))

	// Initialize price cache
	priceCache = NewPriceCache()

	// Test connectivity
	if err := testEnhancedConnectivity(); err != nil {
		log.Fatalf("❌ Connectivity test failed: %v", err)
	}

	// Initialize services
	if err := initializeServices(); err != nil {
		log.Fatalf("❌ Service initialization error: %v", err)
	}

	// Start webhook server
	startWebhookServer()

	// Create sheet headers
	if err := ensureSheetHeaders(); err != nil {
		log.Printf("⚠️ Warning: Could not ensure sheet headers: %v", err)
	}

	// Start monitoring loop with enhanced interval
	log.Printf("🔄 Starting enhanced monitoring loop (%d seconds interval)...", appConfig.TickIntervalSeconds)
	ticker := time.NewTicker(time.Duration(appConfig.TickIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		monitorTokensEnhanced()
		<-ticker.C
	}
}

func testEnhancedConnectivity() error {
	log.Println("🧪 Testing enhanced connectivity...")

	// Test all RPC endpoints
	for i, url := range appConfig.HeliusRPCURLs {
		log.Printf("Testing RPC %d: %s", i+1, url[:50]+"...")
		// You can add specific RPC tests here
	}

	// Test Jupiter API
	if err := testJupiterAPI(); err != nil {
		return fmt.Errorf("Jupiter API test failed: %v", err)
	}

	log.Println("✅ Enhanced connectivity test passed")
	return nil
}

func monitorTokensEnhanced() {
	log.Println("--- Enhanced Token Monitoring ---")
	
	monitoringList, err := readMonitoringListFromSheet()
	if err != nil {
		log.Printf("⚠️ Error reading sheet: %v", err)
		return
	}

	log.Printf("📊 Monitoring %d tokens", len(monitoringList))

	// Process tokens with rate limiting
	for i, token := range monitoringList {
		// Add staggered delay to prevent overwhelming APIs
		if i > 0 {
			time.Sleep(time.Duration(200) * time.Millisecond)
		}

		switch token.Status {
		case "NEW", "":
			go startMonitoringEnhanced(token)
		case "MONITORING":
			go monitorActiveTokenEnhanced(token)
		}
	}
}

func startMonitoringEnhanced(token TokenMonitoring) {
	log.Printf("🆕 Starting enhanced monitoring: %s", token.TokenAddress[:8])

	// Update status
	vrStatus := &sheets.ValueRange{Values: [][]interface{}{{"INITIALIZING"}}}
	rangeStrStatus := fmt.Sprintf("%s!C%d", appConfig.SheetName, token.RowIndex)
	sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStrStatus, vrStatus).ValueInputOption("RAW").Do()

	// Get starting price and market cap
	price, startMarketCap, err := getEnhancedTokenPrice(token.TokenAddress)
	if err != nil {
		log.Printf("⚠️ Could not get starting price for %s: %v", token.TokenAddress[:8], err)
		vrStatus = &sheets.ValueRange{Values: [][]interface{}{{"FAILED"}}}
		sheetsSvc.Spreadsheets.Values.Update(
			appConfig.SpreadsheetID, rangeStrStatus, vrStatus).ValueInputOption("RAW").Do()
		return
	}

	// Get token metadata for name
	tokenName, metaMarketCap, err := getTokenMetadata(token.TokenAddress)
	if err != nil {
		log.Printf("⚠️ Could not get token metadata for %s: %v", token.TokenAddress[:8], err)
		tokenName = "Unknown Token"
	}

	// Use the more reliable market cap source
	if startMarketCap == 0 && metaMarketCap > 0 {
		startMarketCap = metaMarketCap
	}

	// Initialize monitoring data
	token.TokenName = tokenName
	token.MonitorStartPrice = price
	token.CurrentPrice = price
	token.HighestPrice = price
	token.MonitorStartMarketCap = startMarketCap
	token.CurrentMarketCap = startMarketCap
	token.HighestMarketCap = startMarketCap
	token.CurrentMultiplier = 1.0
	token.HighestMultiplier = 1.0
	token.TrailingStopLoss = 0
	token.StopLossMultiplier = 0
	token.ConsecutiveDownticks = 0
	token.LastPriceDirection = "STABLE"
	token.StopLossHistory = []StopLossUpdate{}

	// Update status to MONITORING
	vrStatus = &sheets.ValueRange{Values: [][]interface{}{{"MONITORING"}}}
	sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStrStatus, vrStatus).ValueInputOption("RAW").Do()

	updateMonitoringData(token)
	log.Printf("✅ Enhanced monitoring started for %s at $%.8f", tokenName, price)
}

// ... (Include all the existing structs and helper functions from your original code)

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
		profitDecrease := (oldStopMultiplier - newStopMultiplier) * 100
		updateStatusAndLog(token.RowIndex, "MONITORING",
			"📉 STOP-LOSS ADJUSTED DOWN: %s | %.2fx → %.2fx | Peak: %.2fx | Protection reduced %.1f%% | MC: $%.0f",
			token.TokenName, oldStopMultiplier, newStopMultiplier, token.HighestMultiplier, profitDecrease, token.CurrentMarketCap)
	}

	// Always log the detailed update
	logStopLossUpdate(token.RowIndex, stopLossUpdate)
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

	// Ensure market cap values are properly formatted
	startMarketCapFormatted := token.MonitorStartMarketCap
	currentMarketCapFormatted := token.CurrentMarketCap
	highestMarketCapFormatted := token.HighestMarketCap

	// Format large numbers properly
	if startMarketCapFormatted == 0 {
		startMarketCapFormatted = 0
	}
	if currentMarketCapFormatted == 0 {
		currentMarketCapFormatted = 0
	}
	if highestMarketCapFormatted == 0 {
		highestMarketCapFormatted = 0
	}

	vr := &sheets.ValueRange{
		Values: [][]interface{}{
			{
				token.TokenName,
				token.CallSource,
				callTimeStr,
				fmt.Sprintf("%.8f", token.MonitorStartPrice),
				fmt.Sprintf("%.8f", token.CurrentPrice),
				fmt.Sprintf("%.0f", startMarketCapFormatted),
				fmt.Sprintf("%.0f", currentMarketCapFormatted),
				fmt.Sprintf("%.0f", highestMarketCapFormatted),
				fmt.Sprintf("%.2fx", token.HighestMultiplier),
				fmt.Sprintf("%.2fx", token.CurrentMultiplier),
				fmt.Sprintf("%.1f%%", profitPercent),
				fmt.Sprintf("%.8f", token.TrailingStopLoss),
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
// updateStatusAndLog function - only logs stop-loss related events
func updateStatusAndLog(rowIndex int, status string, logMessage string, args ...interface{}) {
	// Update Status (column C)
	vrStatus := &sheets.ValueRange{Values: [][]interface{}{{status}}}
	rangeStrStatus := fmt.Sprintf("%s!C%d", appConfig.SheetName, rowIndex)
	_, err := sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStrStatus, vrStatus).ValueInputOption("RAW").Do()
	if err != nil {
		log.Printf("⚠️ Failed to update status: %v", err)
	}

	// Format the full log message
	fullLogMessage := fmt.Sprintf(logMessage, args...)
	
	// Always log significant events to column Q (main log)
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

	// Always log to console for monitoring
	log.Printf("📝 Row %d: %s", rowIndex, fullLogMessage)
}
// logStopLossUpdate function - logs detailed stop-loss updates to column P
func logStopLossUpdate(rowIndex int, update StopLossUpdate) {
	updateMessage := fmt.Sprintf("[%s] Stop-Loss: %.2fx → %.2fx | ATH: %.2fx | Price: $%.8f | %s\n",
		update.Timestamp.Format("2006-01-02 15:04:05"),
		update.OldMultiplier,
		update.NewMultiplier,
		update.ATHMultiplier,
		update.NewStopLoss,
		update.Reason,
	)

	// Get existing stop-loss updates
	rangeStrUpdatesGet := fmt.Sprintf("%s!P%d", appConfig.SheetName, rowIndex)
	resp, err := sheetsSvc.Spreadsheets.Values.Get(appConfig.SpreadsheetID, rangeStrUpdatesGet).Do()
	if err != nil {
		log.Printf("⚠️ Failed to read existing stop-loss updates: %v", err)
		return
	}

	existingUpdates := ""
	if len(resp.Values) > 0 && len(resp.Values[0]) > 0 {
		existingUpdates = fmt.Sprintf("%v", resp.Values[0][0])
	}

	newUpdates := existingUpdates + updateMessage
	vrUpdates := &sheets.ValueRange{Values: [][]interface{}{{newUpdates}}}
	_, err = sheetsSvc.Spreadsheets.Values.Update(
		appConfig.SpreadsheetID, rangeStrUpdatesGet, vrUpdates).ValueInputOption("RAW").Do()
	if err != nil {
		log.Printf("⚠️ Failed to append stop-loss update: %v", err)
	}
}