#!/usr/bin/env python3
"""
Enhanced Solana Token Monitoring Bot with Webhooks and RPC Load Balancing
Python implementation maintaining full functionality from Go version
"""

import os
import sys
import json
import time
import logging
import asyncio
from datetime import datetime, timedelta
from typing import List, Dict, Optional, Tuple
from dataclasses import dataclass, field
from threading import Lock
import aiohttp
from aiohttp import web
import requests
from google.oauth2 import service_account
from googleapiclient.discovery import build
from googleapiclient.errors import HttpError

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - %(message)s',
    handlers=[
        logging.StreamHandler(sys.stdout),
        logging.FileHandler('/app/logs/enhanced-monitor.log')
    ]
)
logger = logging.getLogger(__name__)

# --- Core Data Structures ---

@dataclass
class StopLossUpdate:
    timestamp: datetime
    old_stop_loss: float
    new_stop_loss: float
    old_multiplier: float
    new_multiplier: float
    ath_multiplier: float
    reason: str

@dataclass
class TokenMonitoring:
    row_index: int
    token_address: str
    token_name: str = ""
    status: str = "NEW"
    call_source: str = ""
    call_time: Optional[datetime] = None
    monitor_start_price: float = 0.0
    current_price: float = 0.0
    highest_price: float = 0.0
    monitor_start_market_cap: float = 0.0
    current_market_cap: float = 0.0
    highest_market_cap: float = 0.0
    current_multiplier: float = 1.0
    highest_multiplier: float = 1.0
    trailing_stop_loss: float = 0.0
    stop_loss_multiplier: float = 0.0
    last_updated: Optional[datetime] = None
    consecutive_downticks: int = 0
    last_price_direction: str = "STABLE"
    stop_loss_history: List[StopLossUpdate] = field(default_factory=list)

@dataclass
class WebhookPriceUpdate:
    token_address: str
    price: float
    market_cap: float
    timestamp: datetime
    source: str

@dataclass
class Config:
    spreadsheet_id: str
    sheet_name: str
    helius_rpc_urls: List[str]
    tick_interval_seconds: int
    usdc_token_address: str
    slippage_bps: int
    webhook_port: int
    enable_webhooks: bool
    rate_limit_delay: float

# --- Price Cache ---

class PriceCache:
    def __init__(self):
        self._cache: Dict[str, WebhookPriceUpdate] = {}
        self._lock = Lock()
    
    def set(self, token_address: str, update: WebhookPriceUpdate):
        with self._lock:
            self._cache[token_address] = update
    
    def get(self, token_address: str) -> Optional[WebhookPriceUpdate]:
        with self._lock:
            return self._cache.get(token_address)
    
    def is_stale(self, token_address: str, max_age_seconds: float = 30) -> bool:
        with self._lock:
            update = self._cache.get(token_address)
            if update:
                return (datetime.now() - update.timestamp).total_seconds() > max_age_seconds
            return True
    
    def size(self) -> int:
        with self._lock:
            return len(self._cache)

# --- RPC Load Balancer ---

class RPCLoadBalancer:
    def __init__(self, urls: List[str], rate_limit_delay: float):
        self.urls = urls
        self.current_index = 0
        self.rate_limiter: Dict[int, datetime] = {}
        self.rate_limit_delay = rate_limit_delay
        self._lock = Lock()
        logger.info(f"✅ RPC Load Balancer initialized with {len(urls)} endpoints")
    
    def get_next_url(self) -> str:
        with self._lock:
            start_index = self.current_index
            
            while True:
                # Check if this RPC is rate limited
                if self.current_index in self.rate_limiter:
                    last_used = self.rate_limiter[self.current_index]
                    if (datetime.now() - last_used).total_seconds() < self.rate_limit_delay:
                        self.current_index = (self.current_index + 1) % len(self.urls)
                        if self.current_index == start_index:
                            # All RPCs are rate limited, wait
                            time.sleep(0.1)
                        continue
                
                # Use this URL and mark it as used
                url = self.urls[self.current_index]
                self.rate_limiter[self.current_index] = datetime.now()
                self.current_index = (self.current_index + 1) % len(self.urls)
                
                return url

# --- Global State ---

class AppState:
    def __init__(self):
        self.sheets_service = None
        self.rpc_balancer: Optional[RPCLoadBalancer] = None
        self.price_cache = PriceCache()
        self.config: Optional[Config] = None
        self.http_session: Optional[aiohttp.ClientSession] = None
        self.jupiter_quote_url = "https://lite-api.jup.ag/swap/v1/quote"

app_state = AppState()

# --- Configuration ---

def load_config() -> Config:
    logger.info("Loading enhanced configuration...")
    
    def get_required_env(key: str) -> str:
        value = os.getenv(key)
        if not value:
            raise ValueError(f"Required environment variable {key} is not set")
        return value
    
    spreadsheet_id = get_required_env("SPREADSHEET_ID")
    sheet_name = get_required_env("SHEET_NAME")
    usdc_token_address = get_required_env("USDC_TOKEN_ADDRESS")
    
    # Load multiple RPC URLs
    rpc_urls_str = os.getenv("HELIUS_RPC_URLS")
    if rpc_urls_str:
        helius_rpc_urls = [url.strip() for url in rpc_urls_str.split(",")]
    else:
        single_url = get_required_env("HELIUS_RPC_URL")
        helius_rpc_urls = [single_url]
    
    webhook_port = int(os.getenv("WEBHOOK_PORT", "8080"))
    enable_webhooks = os.getenv("ENABLE_WEBHOOKS", "").lower() == "true"
    tick_interval_seconds = int(os.getenv("TICK_INTERVAL_SECONDS", "8"))
    rate_limit_ms = int(os.getenv("RATE_LIMIT_MS", "200"))
    slippage_bps = int(os.getenv("SLIPPAGE_BPS", "100"))
    
    config = Config(
        spreadsheet_id=spreadsheet_id,
        sheet_name=sheet_name,
        helius_rpc_urls=helius_rpc_urls,
        tick_interval_seconds=tick_interval_seconds,
        usdc_token_address=usdc_token_address,
        slippage_bps=slippage_bps,
        webhook_port=webhook_port,
        enable_webhooks=enable_webhooks,
        rate_limit_delay=rate_limit_ms / 1000.0
    )
    
    logger.info(f"✅ Enhanced configuration loaded: {len(helius_rpc_urls)} RPCs, "
                f"Webhooks: {enable_webhooks}, Tick: {tick_interval_seconds}s")
    return config

# --- Service Initialization ---

def initialize_services():
    logger.info("Initializing services...")
    
    cred_path = "credentials.json"
    if not os.path.exists(cred_path):
        raise FileNotFoundError("credentials.json file not found")
    
    credentials = service_account.Credentials.from_service_account_file(
        cred_path,
        scopes=['https://www.googleapis.com/auth/spreadsheets']
    )
    
    app_state.sheets_service = build('sheets', 'v4', credentials=credentials)
    logger.info("✅ Google Sheets service initialized")

def ensure_sheet_headers():
    headers = [[
        "Token Address", "Token Name", "Status", "Call Source", "Call Time",
        "Start Price", "Current Price", "Start MC", "Current MC", "Highest MC",
        "Peak Multiplier", "Current Multiplier", "Profit %", "Stop-Loss Price",
        "Stop-Loss Multiplier", "Stop-Loss Updates", "Activity Log"
    ]]
    
    range_str = f"{app_state.config.sheet_name}!A1:Q1"
    body = {'values': headers}
    
    try:
        app_state.sheets_service.spreadsheets().values().update(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_str,
            valueInputOption='RAW',
            body=body
        ).execute()
        logger.info("✅ Sheet headers ensured")
    except HttpError as e:
        logger.error(f"Failed to create headers: {e}")

# --- Price Fetching ---

async def get_token_metadata(token_address: str) -> Tuple[str, float]:
    """Fetch token metadata from DexScreener"""
    url = f"https://api.dexscreener.com/latest/dex/tokens/{token_address}"
    
    try:
        async with app_state.http_session.get(url, timeout=aiohttp.ClientTimeout(total=10)) as resp:
            if resp.status != 200:
                return "Unknown Token", 0.0
            
            data = await resp.json()
            pairs = data.get('pairs', [])
            
            if not pairs:
                return "Unknown Token", 0.0
            
            pair = pairs[0]
            base_token = pair.get('baseToken', {})
            token_name = base_token.get('name') or base_token.get('symbol', 'Unknown Token')
            market_cap = pair.get('marketCap', 0.0)
            
            return token_name, market_cap
    
    except Exception as e:
        logger.warning(f"DexScreener request failed for {token_address[:8]}: {e}")
        return "Unknown Token", 0.0

async def get_jupiter_price_with_balancer(token_address: str) -> Tuple[float, float]:
    """Get price using Jupiter API with load balancing"""
    await asyncio.sleep(app_state.config.rate_limit_delay)
    
    try:
        price = await get_price_from_quote_balanced(token_address)
        _, market_cap = await get_token_metadata(token_address)
        return price, market_cap
    except Exception as e:
        logger.warning(f"Quote API failed for {token_address[:8]}: {e}")
        return await get_price_from_alternative_endpoints(token_address)

async def get_price_from_quote_balanced(token_address: str) -> float:
    """Try multiple test amounts to get a valid price"""
    test_amounts = ["1000000", "100000", "10000", "1000"]
    
    for i, test_amount in enumerate(test_amounts):
        url = (f"{app_state.jupiter_quote_url}?"
               f"inputMint={token_address}&"
               f"outputMint={app_state.config.usdc_token_address}&"
               f"amount={test_amount}&"
               f"slippageBps={app_state.config.slippage_bps}&"
               f"restrictIntermediateTokens=true")
        
        try:
            async with app_state.http_session.get(url, timeout=aiohttp.ClientTimeout(total=15)) as resp:
                if resp.status == 429:
                    logger.info("⏳ Rate limited, waiting...")
                    await asyncio.sleep(1)
                    if i == len(test_amounts) - 1:
                        raise Exception("Rate limited on all attempts")
                    continue
                
                if resp.status != 200:
                    if i == len(test_amounts) - 1:
                        text = await resp.text()
                        raise Exception(f"Quote failed with status {resp.status}: {text}")
                    continue
                
                quote = await resp.json()
                
                in_amount = float(quote['inAmount'])
                out_amount = float(quote['outAmount'])
                
                if in_amount == 0:
                    continue
                
                price = out_amount / in_amount
                
                if price < 0.000001 and price > 0:
                    price = price * 1000000
                
                if price > 0:
                    return price
        
        except Exception as e:
            if i == len(test_amounts) - 1:
                raise
            continue
    
    raise Exception("Could not get valid price with any test amount")

async def get_price_from_alternative_endpoints(token_address: str) -> Tuple[float, float]:
    """Try reverse quote (USDC -> Token) as fallback"""
    test_usdc_amount = "1000000"
    
    url = (f"{app_state.jupiter_quote_url}?"
           f"inputMint={app_state.config.usdc_token_address}&"
           f"outputMint={token_address}&"
           f"amount={test_usdc_amount}&"
           f"slippageBps={app_state.config.slippage_bps}&"
           f"restrictIntermediateTokens=true")
    
    async with app_state.http_session.get(url, timeout=aiohttp.ClientTimeout(total=15)) as resp:
        if resp.status != 200:
            text = await resp.text()
            raise Exception(f"Reverse quote failed with status {resp.status}: {text}")
        
        quote = await resp.json()
        
        in_amount = float(quote['inAmount'])
        out_amount = float(quote['outAmount'])
        
        if out_amount == 0:
            raise Exception("Invalid token output amount")
        
        price = in_amount / out_amount
        _, market_cap = await get_token_metadata(token_address)
        
        return price, market_cap

async def get_enhanced_token_price(token_address: str) -> Tuple[float, float]:
    """Get token price with webhook cache first, then Jupiter"""
    if app_state.config.enable_webhooks:
        update = app_state.price_cache.get(token_address)
        if update and not app_state.price_cache.is_stale(token_address, 30):
            logger.info(f"🔄 Using cached price for {token_address[:8]}: ${update.price:.8f}")
            return update.price, update.market_cap
    
    return await get_jupiter_price_with_balancer(token_address)

# --- Stop-Loss Calculations ---

def calculate_trailing_stop_loss(token: TokenMonitoring) -> Tuple[float, float]:
    """Calculate trailing stop-loss based on highest multiplier"""
    if token.monitor_start_price <= 0:
        return 0.0, 0.0
    
    highest_multiplier = token.highest_price / token.monitor_start_price
    
    if highest_multiplier < 2.0:
        return 0.0, 0.0
    
    # Progressive stop-loss logic
    if highest_multiplier >= 500:
        base_stop_loss_multiplier = highest_multiplier * 0.88
    elif highest_multiplier >= 100:
        base_stop_loss_multiplier = highest_multiplier * 0.87
    elif highest_multiplier >= 50:
        base_stop_loss_multiplier = highest_multiplier * 0.86
    elif highest_multiplier >= 20:
        base_stop_loss_multiplier = highest_multiplier * 0.85
    elif highest_multiplier >= 10:
        base_stop_loss_multiplier = highest_multiplier * 0.83
    elif highest_multiplier >= 4:
        base_stop_loss_multiplier = highest_multiplier * 0.80
    elif highest_multiplier >= 2:
        base_stop_loss_multiplier = max(highest_multiplier * 0.78, 1.3)
    else:
        return 0.0, 0.0
    
    # Volatility adjustment
    adjusted_stop_loss_multiplier = base_stop_loss_multiplier
    if token.consecutive_downticks >= 3:
        adjusted_stop_loss_multiplier = base_stop_loss_multiplier * 0.95
    
    # Ensure minimum profitable levels
    if highest_multiplier >= 10:
        adjusted_stop_loss_multiplier = max(adjusted_stop_loss_multiplier, 5.0)
    elif highest_multiplier >= 5:
        adjusted_stop_loss_multiplier = max(adjusted_stop_loss_multiplier, 2.5)
    elif highest_multiplier >= 2:
        adjusted_stop_loss_multiplier = max(adjusted_stop_loss_multiplier, 1.2)
    
    stop_loss_price = token.monitor_start_price * adjusted_stop_loss_multiplier
    return stop_loss_price, adjusted_stop_loss_multiplier

# --- Logging Functions ---

def log_ath_achievement(token: TokenMonitoring):
    """Log All-Time High achievements"""
    multiplier = token.highest_multiplier
    
    if multiplier >= 100.0:
        emoji = "🚀🚀🚀"
        message = "LEGENDARY ATH!"
        protection = multiplier * 0.87
    elif multiplier >= 50.0:
        emoji = "🚀🚀"
        message = "MASSIVE ATH!"
        protection = multiplier * 0.86
    elif multiplier >= 20.0:
        emoji = "🚀"
        message = "HUGE ATH!"
        protection = multiplier * 0.85
    elif multiplier >= 10.0:
        emoji = "🔥"
        message = "MAJOR ATH!"
        protection = multiplier * 0.83
    elif multiplier >= 4.0:
        emoji = "📈"
        message = "STRONG ATH!"
        protection = multiplier * 0.80
    elif multiplier >= 2.0:
        emoji = "✅"
        message = "NEW ATH!"
        protection = max(multiplier * 0.78, 1.3)
    elif multiplier >= 1.5:
        update_status_and_log(
            token.row_index, "MONITORING",
            f"📊 Moving Up! {token.token_name} | {multiplier:.2f}x | "
            f"Price: ${token.current_price:.8f} | MC: ${token.highest_market_cap:.0f} | "
            f"No protection yet (need 2x)"
        )
        return
    else:
        return
    
    secured_profit = (protection - 1) * 100
    update_status_and_log(
        token.row_index, "MONITORING",
        f"{emoji} {message} {token.token_name} | {multiplier:.1f}x | "
        f"Price: ${token.current_price:.8f} | MC: ${token.highest_market_cap:.0f} | "
        f"Protected at: {protection:.1f}x ({secured_profit:.0f}% secured)"
    )

def log_multiplier_milestones(token: TokenMonitoring, previous_multiplier: float, current_multiplier: float):
    """Log crossing of significant multiplier milestones"""
    milestones = [0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0, 1.2, 1.5, 1.8, 2.0, 
                  2.5, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0, 10.0, 15.0, 20.0, 25.0, 30.0, 
                  40.0, 50.0, 75.0, 100.0, 150.0, 200.0, 300.0, 500.0, 1000.0]
    
    for milestone in milestones:
        # Crossing upward
        if previous_multiplier < milestone <= current_multiplier:
            log_milestone_event(token, milestone, "UP")
        # Crossing downward
        elif previous_multiplier > milestone >= current_multiplier:
            log_milestone_event(token, milestone, "DOWN")

def log_milestone_event(token: TokenMonitoring, milestone: float, direction: str):
    """Log specific milestone crossing events"""
    if direction == "UP":
        if milestone >= 100:
            emoji, message = "🚀🚀🚀", "LEGENDARY MILESTONE"
        elif milestone >= 50:
            emoji, message = "🚀🚀", "MASSIVE MILESTONE"
        elif milestone >= 10:
            emoji, message = "🚀", "MAJOR MILESTONE"
        elif milestone >= 2:
            emoji, message = "🔥", "STRONG MILESTONE"
        elif milestone >= 1:
            emoji, message = "✅", "BREAKEVEN+"
        elif milestone >= 0.5:
            emoji, message = "📈", "RECOVERY"
        else:
            emoji, message = "📊", "MILESTONE"
        
        update_status_and_log(
            token.row_index, "MONITORING",
            f"{emoji} {message} REACHED! {token.token_name} | {milestone:.2f}x | "
            f"Price: ${token.current_price:.8f} | MC: ${token.current_market_cap:.0f} | "
            f"Peak: {token.highest_multiplier:.2f}x"
        )
    else:
        if milestone >= 10:
            emoji, message = "📉", "MAJOR PULLBACK"
        elif milestone >= 2:
            emoji, message = "⚠️", "PULLBACK"
        elif milestone >= 1:
            emoji, message = "🔴", "BELOW BREAKEVEN"
        elif milestone >= 0.5:
            emoji, message = "💔", "MAJOR LOSS"
        else:
            emoji, message = "📉", "DECLINE"
        
        percent_from_peak = ((token.highest_multiplier - milestone) / token.highest_multiplier) * 100
        update_status_and_log(
            token.row_index, "MONITORING",
            f"{emoji} {message}: {token.token_name} | {milestone:.2f}x | "
            f"Price: ${token.current_price:.8f} | MC: ${token.current_market_cap:.0f} | "
            f"Down {percent_from_peak:.1f}% from peak {token.highest_multiplier:.2f}x"
        )

def handle_stop_loss_update(token: TokenMonitoring, old_stop_loss: float, new_stop_loss: float,
                            old_stop_multiplier: float, new_stop_multiplier: float, is_new_ath: bool):
    """Handle and log stop-loss updates"""
    if is_new_ath:
        reason = f"ATH reached {token.highest_multiplier:.2f}x"
    elif new_stop_loss > old_stop_loss:
        reason = "Stop-loss raised due to price movement"
    elif new_stop_loss < old_stop_loss:
        reason = "Stop-loss lowered due to price decline"
    elif old_stop_loss == 0 and new_stop_loss > 0:
        reason = "Stop-loss activated at 2x"
    else:
        reason = "Stop-loss recalculated"
    
    stop_loss_update = StopLossUpdate(
        timestamp=datetime.now(),
        old_stop_loss=old_stop_loss,
        new_stop_loss=new_stop_loss,
        old_multiplier=old_stop_multiplier,
        new_multiplier=new_stop_multiplier,
        ath_multiplier=token.highest_multiplier,
        reason=reason
    )
    token.stop_loss_history.append(stop_loss_update)
    
    # Log different types of stop-loss events
    if old_stop_loss == 0 and new_stop_loss > 0:
        secured_profit = (new_stop_multiplier - 1) * 100
        update_status_and_log(
            token.row_index, "MONITORING",
            f"✅ TRAILING STOP-LOSS ACTIVATED! {token.token_name} | "
            f"Protected at {new_stop_multiplier:.2f}x | Peak: {token.highest_multiplier:.2f}x | "
            f"Securing +{secured_profit:.0f}% profit | MC: ${token.current_market_cap:.0f}"
        )
    elif new_stop_loss > old_stop_loss:
        profit_increase = (new_stop_multiplier - old_stop_multiplier) * 100
        update_status_and_log(
            token.row_index, "MONITORING",
            f"📈 STOP-LOSS RAISED: {token.token_name} | {old_stop_multiplier:.2f}x → {new_stop_multiplier:.2f}x | "
            f"Peak: {token.highest_multiplier:.2f}x | Profit secured +{profit_increase:.1f}% more | "
            f"MC: ${token.current_market_cap:.0f}"
        )
    elif new_stop_loss < old_stop_loss and new_stop_loss > 0:
        profit_decrease = (old_stop_multiplier - new_stop_multiplier) * 100
        update_status_and_log(
            token.row_index, "MONITORING",
            f"📉 STOP-LOSS ADJUSTED DOWN: {token.token_name} | {old_stop_multiplier:.2f}x → {new_stop_multiplier:.2f}x | "
            f"Peak: {token.highest_multiplier:.2f}x | Protection reduced {profit_decrease:.1f}% | "
            f"MC: ${token.current_market_cap:.0f}"
        )
    
    log_stop_loss_update(token.row_index, stop_loss_update)

def log_stop_loss_update(row_index: int, update: StopLossUpdate):
    """Log detailed stop-loss update to column P"""
    update_message = (
        f"[{update.timestamp.strftime('%Y-%m-%d %H:%M:%S')}] "
        f"Stop-Loss: {update.old_multiplier:.2f}x → {update.new_multiplier:.2f}x | "
        f"ATH: {update.ath_multiplier:.2f}x | Price: ${update.new_stop_loss:.8f} | {update.reason}\n"
    )
    
    try:
        # Get existing updates
        range_str = f"{app_state.config.sheet_name}!P{row_index}"
        result = app_state.sheets_service.spreadsheets().values().get(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_str
        ).execute()
        
        existing_updates = ""
        if 'values' in result and result['values'] and result['values'][0]:
            existing_updates = result['values'][0][0]
        
        new_updates = existing_updates + update_message
        body = {'values': [[new_updates]]}
        
        app_state.sheets_service.spreadsheets().values().update(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_str,
            valueInputOption='RAW',
            body=body
        ).execute()
    
    except Exception as e:
        logger.warning(f"Failed to append stop-loss update: {e}")

def update_status_and_log(row_index: int, status: str, log_message: str):
    """Update status and append to activity log"""
    try:
        # Update Status (column C)
        range_status = f"{app_state.config.sheet_name}!C{row_index}"
        body_status = {'values': [[status]]}
        app_state.sheets_service.spreadsheets().values().update(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_status,
            valueInputOption='RAW',
            body=body_status
        ).execute()
        
        # Append to Activity Log (column Q)
        timestamped_log = f"[{datetime.now().strftime('%Y-%m-%d %H:%M:%S')}] {log_message}\n"
        
        range_log = f"{app_state.config.sheet_name}!Q{row_index}"
        result = app_state.sheets_service.spreadsheets().values().get(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_log
        ).execute()
        
        existing_log = ""
        if 'values' in result and result['values'] and result['values'][0]:
            existing_log = result['values'][0][0]
        
        new_log = existing_log + timestamped_log
        body_log = {'values': [[new_log]]}
        
        app_state.sheets_service.spreadsheets().values().update(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_log,
            valueInputOption='RAW',
            body=body_log
        ).execute()
        
        logger.info(f"📝 Row {row_index}: {log_message}")
    
    except Exception as e:
        logger.warning(f"Failed to update status and log: {e}")

# --- Sheet Operations ---

def read_monitoring_list_from_sheet() -> List[TokenMonitoring]:
    """Read all tokens from the monitoring sheet"""
    try:
        range_str = f"{app_state.config.sheet_name}!A2:Q"
        result = app_state.sheets_service.spreadsheets().values().get(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_str
        ).execute()
        
        values = result.get('values', [])
        tokens = []
        
        for i, row in enumerate(values):
            if not row or not row[0]:
                continue
            
            token = TokenMonitoring(
                row_index=i + 2,
                token_address=str(row[0])
            )
            
            if len(row) > 1: token.token_name = str(row[1])
            if len(row) > 2: token.status = str(row[2])
            if len(row) > 3: token.call_source = str(row[3])
            if len(row) > 4 and row[4]:
                try:
                    token.call_time = datetime.strptime(str(row[4]), "%Y-%m-%d %H:%M:%S")
                except:
                    pass
            if len(row) > 5: 
                try:
                    token.monitor_start_price = float(row[5])
                except:
                    pass
            if len(row) > 6:
                try:
                    token.current_price = float(row[6])
                except:
                    pass
            if len(row) > 7:
                try:
                    token.monitor_start_market_cap = float(row[7])
                except:
                    pass
            if len(row) > 8:
                try:
                    token.current_market_cap = float(row[8])
                except:
                    pass
            if len(row) > 9:
                try:
                    token.highest_market_cap = float(row[9])
                except:
                    pass
            if len(row) > 10:
                try:
                    peak_str = str(row[10]).rstrip('x')
                    token.highest_multiplier = float(peak_str)
                except:
                    pass
            if len(row) > 11:
                try:
                    current_str = str(row[11]).rstrip('x')
                    token.current_multiplier = float(current_str)
                except:
                    pass
            if len(row) > 13:
                try:
                    token.trailing_stop_loss = float(row[13])
                except:
                    pass
            if len(row) > 14:
                try:
                    stop_str = str(row[14]).rstrip('x')
                    token.stop_loss_multiplier = float(stop_str)
                except:
                    pass
            
            if not token.status:
                token.status = "NEW"
            
            tokens.append(token)
        
        return tokens
    
    except Exception as e:
        logger.error(f"Failed to read monitoring list: {e}")
        return []

def update_monitoring_data(token: TokenMonitoring):
    """Update token monitoring data in the sheet"""
    try:
        profit_percent = 0.0
        if token.monitor_start_price > 0:
            profit_percent = ((token.current_price / token.monitor_start_price) - 1) * 100
        
        call_time_str = ""
        if token.call_time:
            call_time_str = token.call_time.strftime("%Y-%m-%d %H:%M:%S")
        
        values = [[
            token.token_name,
            token.call_source,
            call_time_str,
            f"{token.monitor_start_price:.8f}",
            f"{token.current_price:.8f}",
            f"{token.monitor_start_market_cap:.0f}",
            f"{token.current_market_cap:.0f}",
            f"{token.highest_market_cap:.0f}",
            f"{token.highest_multiplier:.2f}x",
            f"{token.current_multiplier:.2f}x",
            f"{profit_percent:.1f}%",
            f"{token.trailing_stop_loss:.8f}",
            f"{token.stop_loss_multiplier:.2f}x"
        ]]
        
        range_str = f"{app_state.config.sheet_name}!B{token.row_index}:N{token.row_index}"
        body = {'values': values}
        
        app_state.sheets_service.spreadsheets().values().update(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_str,
            valueInputOption='USER_ENTERED',
            body=body
        ).execute()
    
    except Exception as e:
        logger.warning(f"Failed to update monitoring data: {e}")

# --- Monitoring Logic ---

async def start_monitoring_enhanced(token: TokenMonitoring):
    """Initialize monitoring for a new token"""
    logger.info(f"🆕 Starting enhanced monitoring: {token.token_address[:8]}")
    
    # Update status to INITIALIZING
    try:
        range_status = f"{app_state.config.sheet_name}!C{token.row_index}"
        body = {'values': [["INITIALIZING"]]}
        app_state.sheets_service.spreadsheets().values().update(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_status,
            valueInputOption='RAW',
            body=body
        ).execute()
    except Exception as e:
        logger.warning(f"Failed to update status: {e}")
    
    # Get starting price and market cap
    try:
        price, start_market_cap = await get_enhanced_token_price(token.token_address)
    except Exception as e:
        logger.error(f"Could not get starting price for {token.token_address[:8]}: {e}")
        try:
            range_status = f"{app_state.config.sheet_name}!C{token.row_index}"
            body = {'values': [["FAILED"]]}
            app_state.sheets_service.spreadsheets().values().update(
                spreadsheetId=app_state.config.spreadsheet_id,
                range=range_status,
                valueInputOption='RAW',
                body=body
            ).execute()
        except:
            pass
        return
    
    # Get token metadata
    token_name, meta_market_cap = await get_token_metadata(token.token_address)
    
    # Use more reliable market cap
    if start_market_cap == 0 and meta_market_cap > 0:
        start_market_cap = meta_market_cap
    
    # Initialize monitoring data
    token.token_name = token_name
    token.monitor_start_price = price
    token.current_price = price
    token.highest_price = price
    token.monitor_start_market_cap = start_market_cap
    token.current_market_cap = start_market_cap
    token.highest_market_cap = start_market_cap
    token.current_multiplier = 1.0
    token.highest_multiplier = 1.0
    token.trailing_stop_loss = 0.0
    token.stop_loss_multiplier = 0.0
    token.consecutive_downticks = 0
    token.last_price_direction = "STABLE"
    token.stop_loss_history = []
    
    # Update status to MONITORING
    try:
        range_status = f"{app_state.config.sheet_name}!C{token.row_index}"
        body = {'values': [["MONITORING"]]}
        app_state.sheets_service.spreadsheets().values().update(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_status,
            valueInputOption='RAW',
            body=body
        ).execute()
    except:
        pass
    
    update_monitoring_data(token)
    logger.info(f"✅ Enhanced monitoring started for {token_name} at ${price:.8f}")

async def monitor_active_token_enhanced(token: TokenMonitoring):
    """Monitor an active token and update its data"""
    logger.info(f"👀 Enhanced monitoring: {token.token_name} ({token.token_address[:8]}) - "
                f"Current: {token.current_multiplier:.2f}x")
    
    # Get current price and market cap
    try:
        price, current_market_cap = await get_enhanced_token_price(token.token_address)
    except Exception as e:
        logger.warning(f"Price fetch failed for {token.token_address[:8]}: {e}")
        return
    
    # Track price direction
    previous_price = token.current_price
    previous_multiplier = token.current_multiplier
    
    if price > previous_price:
        price_direction = "UP"
        token.consecutive_downticks = 0
    elif price < previous_price:
        price_direction = "DOWN"
        if token.last_price_direction == "DOWN":
            token.consecutive_downticks += 1
        else:
            token.consecutive_downticks = 1
    else:
        price_direction = "STABLE"
    
    token.last_price_direction = price_direction
    
    # Update position
    token.current_price = price
    token.current_market_cap = current_market_cap
    token.last_updated = datetime.now()
    
    # Calculate current multiplier
    if token.monitor_start_price > 0:
        token.current_multiplier = token.current_price / token.monitor_start_price
    
    # Update highest price and market cap
    is_new_ath = False
    if token.current_price > token.highest_price:
        token.highest_price = token.current_price
        token.highest_market_cap = current_market_cap
        token.highest_multiplier = token.current_multiplier
        is_new_ath = True
        log_ath_achievement(token)
    
    # Calculate and update trailing stop-loss
    old_stop_loss = token.trailing_stop_loss
    old_stop_multiplier = token.stop_loss_multiplier
    new_stop_loss, new_stop_multiplier = calculate_trailing_stop_loss(token)
    
    # Log multiplier milestones
    log_multiplier_milestones(token, previous_multiplier, token.current_multiplier)
    
    # Track stop-loss updates
    if new_stop_loss != old_stop_loss or new_stop_multiplier != old_stop_multiplier:
        handle_stop_loss_update(token, old_stop_loss, new_stop_loss, 
                               old_stop_multiplier, new_stop_multiplier, is_new_ath)
    
    token.trailing_stop_loss = new_stop_loss
    token.stop_loss_multiplier = new_stop_multiplier
    
    # Check for stop-loss trigger
    if token.trailing_stop_loss > 0 and token.current_price <= token.trailing_stop_loss:
        percent_drop = ((token.highest_multiplier - token.current_multiplier) / 
                       token.highest_multiplier) * 100
        update_status_and_log(
            token.row_index, "STOP-LOSS TRIGGERED",
            f"🛑 STOP-LOSS TRIGGERED! {token.token_name} | Current: {token.current_multiplier:.2f}x | "
            f"Stop: {token.stop_loss_multiplier:.2f}x | Peak was: {token.highest_multiplier:.2f}x | "
            f"Dropped {percent_drop:.1f}% from peak | MC: ${token.current_market_cap:.0f}"
        )
    
    # Update sheet
    update_monitoring_data(token)

async def monitor_tokens_enhanced():
    """Main monitoring loop for all tokens"""
    logger.info("--- Enhanced Token Monitoring ---")
    
    monitoring_list = read_monitoring_list_from_sheet()
    logger.info(f"📊 Monitoring {len(monitoring_list)} tokens")
    
    # Process tokens with staggered delays
    tasks = []
    for i, token in enumerate(monitoring_list):
        if i > 0:
            await asyncio.sleep(0.2)  # 200ms delay between tokens
        
        if token.status in ["NEW", ""]:
            tasks.append(start_monitoring_enhanced(token))
        elif token.status == "MONITORING":
            tasks.append(monitor_active_token_enhanced(token))
    
    # Execute all monitoring tasks concurrently
    if tasks:
        await asyncio.gather(*tasks, return_exceptions=True)

# --- Webhook Server ---

async def handle_price_webhook(request):
    """Handle incoming price webhook updates"""
    try:
        data = await request.json()
        
        update = WebhookPriceUpdate(
            token_address=data['token_address'],
            price=float(data['price']),
            market_cap=float(data['market_cap']),
            timestamp=datetime.now(),
            source=data.get('source', 'webhook')
        )
        
        app_state.price_cache.set(update.token_address, update)
        logger.info(f"📡 Webhook price update: {update.token_address[:8]} = ${update.price:.8f}")
        
        return web.Response(text="OK")
    
    except Exception as e:
        logger.error(f"Webhook error: {e}")
        return web.Response(text="Error", status=400)

async def handle_health_check(request):
    """Health check endpoint"""
    status = {
        'status': 'healthy',
        'timestamp': datetime.now().isoformat(),
        'rpc_urls': len(app_state.config.helius_rpc_urls),
        'webhooks': app_state.config.enable_webhooks,
        'cache_size': app_state.price_cache.size()
    }
    return web.json_response(status)

async def start_webhook_server():
    """Start the webhook HTTP server"""
    if not app_state.config.enable_webhooks:
        return
    
    app = web.Application()
    app.router.add_post('/webhook/price', handle_price_webhook)
    app.router.add_get('/health', handle_health_check)
    
    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, '0.0.0.0', app_state.config.webhook_port)
    await site.start()
    
    logger.info(f"🌐 Webhook server started on port {app_state.config.webhook_port}")

# --- Main Application ---

async def main_async():
    """Main async application loop"""
    logger.info("🚀 Starting Enhanced Solana Token Monitoring Bot...")
    
    # Load configuration
    app_state.config = load_config()
    
    # Initialize RPC load balancer
    app_state.rpc_balancer = RPCLoadBalancer(
        app_state.config.helius_rpc_urls,
        app_state.config.rate_limit_delay
    )
    
    # Initialize services
    initialize_services()
    
    # Create HTTP session
    app_state.http_session = aiohttp.ClientSession()
    
    # Test connectivity
    logger.info("🧪 Testing connectivity...")
    try:
        async with app_state.http_session.get(
            f"{app_state.jupiter_quote_url}?inputMint=So11111111111111111111111111111111111111112&"
            f"outputMint={app_state.config.usdc_token_address}&amount=1000000&slippageBps=100",
            timeout=aiohttp.ClientTimeout(total=10)
        ) as resp:
            if resp.status == 200:
                logger.info("✅ Jupiter API connectivity test passed")
    except Exception as e:
        logger.warning(f"Jupiter API test failed: {e}")
    
    # Ensure sheet headers
    ensure_sheet_headers()
    
    # Start webhook server
    if app_state.config.enable_webhooks:
        await start_webhook_server()
    
    # Main monitoring loop
    logger.info(f"🔄 Starting enhanced monitoring loop ({app_state.config.tick_interval_seconds}s interval)...")
    
    try:
        while True:
            await monitor_tokens_enhanced()
            await asyncio.sleep(app_state.config.tick_interval_seconds)
    
    except KeyboardInterrupt:
        logger.info("Shutting down...")
    
    finally:
        if app_state.http_session:
            await app_state.http_session.close()

def main():
    """Entry point"""
    try:
        asyncio.run(main_async())
    except KeyboardInterrupt:
        logger.info("Application stopped by user")
    except Exception as e:
        logger.error(f"Fatal error: {e}", exc_info=True)
        sys.exit(1)

if __name__ == "__main__":
    main()