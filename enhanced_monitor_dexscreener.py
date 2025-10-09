#!/usr/bin/env python3
"""
Enhanced Solana Token Monitoring Bot - DexScreener Real-Time Version.
FIXED: Batched updates to avoid Google Sheets API rate limits and removed duplicated/syntax errors.
"""

import os
import sys
import json
import time
import logging
import asyncio
from datetime import datetime
from typing import List, Dict, Optional, Tuple
from dataclasses import dataclass, field
from threading import Lock
import aiohttp
from google.oauth2 import service_account
from googleapiclient.discovery import build
from googleapiclient.errors import HttpError

# Ensure log dir exists
LOG_DIR = "/app/logs"
if not os.path.exists(LOG_DIR):
    try:
        os.makedirs(LOG_DIR, exist_ok=True)
    except Exception:
        # Fallback to stdout-only logging if permission denied
        LOG_DIR = None

# Configure logging
handlers = [logging.StreamHandler(sys.stdout)]
if LOG_DIR:
    handlers.append(logging.FileHandler(os.path.join(LOG_DIR, 'dexscreener-monitor.log')))

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - %(message)s',
    handlers=handlers
)
logger = logging.getLogger(__name__)

# --- Core Data Structures ---

@dataclass
class MultiplierMilestone:
    timestamp: datetime
    multiplier: float
    direction: str  # "UP" or "DOWN"
    price: float
    market_cap: float

@dataclass
class TokenMonitoring:
    row_index: int
    token_address: str
    token_name: str = ""
    status: str = "NEW"
    monitor_start_price: float = 0.0
    current_price: float = 0.0
    highest_price: float = 0.0
    lowest_price: float = 0.0
    monitor_start_market_cap: float = 0.0
    current_market_cap: float = 0.0
    highest_market_cap: float = 0.0
    current_multiplier: float = 1.0
    highest_multiplier: float = 1.0
    lowest_multiplier: float = 1.0
    last_updated: Optional[datetime] = None
    milestone_history: List[MultiplierMilestone] = field(default_factory=list)
    last_logged_milestone: float = 0.0
    initial_write_done: bool = False
    needs_update: bool = False  # NEW: Track if update is needed

@dataclass
class Config:
    spreadsheet_id: str
    sheet_name: str
    tick_interval_seconds: float
    dexscreener_api_url: str
    rate_limit_delay: float
    batch_update_interval: int  # NEW: How many ticks between forced updates
    max_requests_per_minute: int  # NEW: DexScreener rate limit

# --- Global State ---

class AppState:
    def __init__(self):
        self.sheets_service = None
        self.config: Optional[Config] = None
        self.http_session: Optional[aiohttp.ClientSession] = None
        self.monitored_tokens: Dict[str, TokenMonitoring] = {}
        self._lock = Lock()
        self.tick_counter = 0  # NEW: Count ticks for periodic updates
        self.api_calls_this_minute = 0  # NEW: Track API calls
        self.last_minute_reset = datetime.now()  # NEW: Track when to reset counter

app_state = AppState()

# --- Configuration ---

def load_config() -> Config:
    logger.info("Loading configuration...")
    
    def get_required_env(key: str) -> str:
        value = os.getenv(key)
        if not value:
            raise ValueError(f"Required environment variable {key} is not set")
        return value
    
    spreadsheet_id = get_required_env("SPREADSHEET_ID")
    sheet_name = get_required_env("SHEET_NAME")
    
    tick_interval_seconds = float(os.getenv("TICK_INTERVAL_SECONDS", "1"))
    rate_limit_delay = float(os.getenv("RATE_LIMIT_MS", "100")) / 1000.0
    batch_update_interval = int(os.getenv("BATCH_UPDATE_INTERVAL", "60"))  # Update all every 60 ticks
    max_requests_per_minute = int(os.getenv("MAX_DEXSCREENER_REQUESTS_PER_MIN", "250"))  # Conservative limit
    dexscreener_api_url = os.getenv("DEXSCREENER_API_URL", "https://api.dexscreener.com/tokens/v1/solana/")

    config = Config(
        spreadsheet_id=spreadsheet_id,
        sheet_name=sheet_name,
        tick_interval_seconds=tick_interval_seconds,
        dexscreener_api_url=dexscreener_api_url,
        rate_limit_delay=rate_limit_delay,
        batch_update_interval=batch_update_interval,
        max_requests_per_minute=max_requests_per_minute
    )
    
    logger.info(f"✅ Configuration loaded: Tick: {tick_interval_seconds}s, Batch: {batch_update_interval}, API limit: {max_requests_per_minute}/min")
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
    """Create headers for the monitoring sheet"""
    headers = [[
        "Token Address", "Token Name", "Status", "Start Price", "Current Price", 
        "Start MC", "Current MC", "Highest MC", "Current Multiplier",
        "Lowest Multiplier", "Profit %", "Multiplier Tracking"
    ]]
    
    range_str = f"{app_state.config.sheet_name}!A1:L1"
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

# --- DexScreener Price Fetching ---

async def get_dexscreener_data_batch(token_addresses: List[str]) -> Dict[str, Tuple[str, float, float]]:
    """
    OPTIMIZED: Fetch up to 30 tokens in a SINGLE API call
    Returns dict: {token_address: (name, price, market_cap)}
    """
    if not token_addresses:
        return {}
    
    # Check and reset rate limit counter
    now = datetime.now()
    if (now - app_state.last_minute_reset).total_seconds() >= 60:
        app_state.api_calls_this_minute = 0
        app_state.last_minute_reset = now
        logger.debug("✅ Rate limit counter reset")
    
    # DexScreener supports up to 30 tokens per request
    batch_size = 30
    all_results: Dict[str, Tuple[str, float, float]] = {}
    
    for i in range(0, len(token_addresses), batch_size):
        # Recompute now for accurate wait calculation
        now = datetime.now()
        if app_state.api_calls_this_minute >= app_state.config.max_requests_per_minute:
            wait_time = 60 - (now - app_state.last_minute_reset).total_seconds()
            if wait_time > 0:
                logger.warning(f"⏸️ Rate limit approaching, waiting {wait_time:.1f}s...")
                await asyncio.sleep(wait_time)
                app_state.api_calls_this_minute = 0
                app_state.last_minute_reset = datetime.now()
        
        batch = token_addresses[i:i + batch_size]
        addresses_str = ",".join(batch)
        
        # Use the BATCH endpoint via configured base URL
        url = f"{app_state.config.dexscreener_api_url}{addresses_str}"
        
        try:
            app_state.api_calls_this_minute += 1
            logger.debug(f"🌐 API call #{app_state.api_calls_this_minute} this minute -> {url}")
            
            async with app_state.http_session.get(url, timeout=aiohttp.ClientTimeout(total=10)) as resp:
                if resp.status == 429:
                    logger.error("⚠️ DexScreener rate limit hit! Waiting 60s...")
                    await asyncio.sleep(60)
                    app_state.api_calls_this_minute = 0
                    app_state.last_minute_reset = datetime.now()
                    # Mark all batch as unknown and continue
                    for addr in batch:
                        all_results[addr] = ("Unknown Token", 0.0, 0.0)
                    continue
                
                if resp.status != 200:
                    logger.warning(f"DexScreener batch returned status {resp.status}")
                    # Mark all as failed
                    for addr in batch:
                        all_results[addr] = ("Unknown Token", 0.0, 0.0)
                    continue
                
                data = await resp.json()
                pairs_list = data if isinstance(data, list) else data.get('pairs', [])
                
                # Create a map of token addresses to their best pair data
                token_data_map: Dict[str, Tuple[str, float, float, float]] = {}
                for pair in pairs_list:
                    base_token = pair.get('baseToken', {})
                    token_addr = base_token.get('address', '').strip()
                    
                    if not token_addr:
                        continue
                    
                    # Get token info
                    token_name = base_token.get('name') or base_token.get('symbol', 'Unknown Token')
                    try:
                        price_usd = float(pair.get('priceUsd', 0) or 0)
                    except (TypeError, ValueError):
                        price_usd = 0.0
                    try:
                        market_cap = float(pair.get('fdv', 0) or pair.get('marketCap', 0) or 0)
                    except (TypeError, ValueError):
                        market_cap = 0.0
                    try:
                        liquidity = float((pair.get('liquidity') or {}).get('usd', 0) or 0)
                    except (TypeError, ValueError):
                        liquidity = 0.0
                    
                    # Keep the pair with highest liquidity for each token
                    if token_addr not in token_data_map or liquidity > token_data_map[token_addr][3]:
                        token_data_map[token_addr] = (token_name, price_usd, market_cap, liquidity)
                
                # Map results back to requested addresses
                for addr in batch:
                    if addr in token_data_map:
                        name, price, mc, _ = token_data_map[addr]
                        all_results[addr] = (name, price, mc)
                    else:
                        all_results[addr] = ("Unknown Token", 0.0, 0.0)
                        logger.warning(f"No data found for {addr[:8]}")
        
        except asyncio.TimeoutError:
            logger.warning(f"Timeout fetching batch of {len(batch)} tokens")
            for addr in batch:
                all_results[addr] = ("Unknown Token", 0.0, 0.0)
        except Exception as e:
            logger.warning(f"Batch request failed: {e}")
            for addr in batch:
                all_results[addr] = ("Unknown Token", 0.0, 0.0)
        
        # Small delay between batches if needed
        if i + batch_size < len(token_addresses):
            await asyncio.sleep(app_state.config.rate_limit_delay)
    
    return all_results

async def get_dexscreener_data(token_address: str) -> Tuple[str, float, float]:
    """
    Single token fetch (fallback for individual use)
    For efficiency, use get_dexscreener_data_batch() instead
    """
    result = await get_dexscreener_data_batch([token_address])
    return result.get(token_address, ("Unknown Token", 0.0, 0.0))

# --- Milestone Tracking ---

def get_milestone_thresholds() -> List[float]:
    """Define multiplier milestones to track"""
    milestones = []
    milestones.extend([0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9])
    milestones.extend([1.0, 1.1, 1.2, 1.3, 1.4, 1.5, 1.6, 1.7, 1.8, 1.9])
    milestones.extend([2.0, 2.2, 2.4, 2.6, 2.8, 3.0])
    milestones.extend([3.4, 3.8, 4.2, 4.6, 5.0, 5.4, 5.8, 6.2, 6.6, 7.0, 7.4, 7.8, 8.2, 8.6, 9.0, 9.4, 9.8, 10.0])
    milestones.extend([11.0, 12.0, 13.0, 14.0, 15.0, 16.0, 17.0, 18.0, 19.0, 20.0])
    milestones.extend([25.0, 30.0, 35.0, 40.0, 45.0, 50.0])
    milestones.extend([60.0, 70.0, 80.0, 90.0, 100.0])
    milestones.extend([150.0, 200.0, 250.0, 300.0, 400.0, 500.0, 1000.0])
    return milestones

def find_crossed_milestones(old_multiplier: float, new_multiplier: float) -> List[Tuple[float, str]]:
    """Find all milestones crossed between old and new multiplier"""
    milestones = get_milestone_thresholds()
    crossed = []
    
    if new_multiplier > old_multiplier:
        for milestone in milestones:
            if old_multiplier < milestone <= new_multiplier:
                crossed.append((milestone, "UP"))
    else:
        for milestone in reversed(milestones):
            if new_multiplier < milestone <= old_multiplier:
                crossed.append((milestone, "DOWN"))
    
    return crossed

def format_milestone_tracking(token: TokenMonitoring) -> str:
    """Format milestone history for spreadsheet display"""
    if not token.milestone_history:
        return ""
    
    recent_milestones = token.milestone_history[-20:]
    lines = []
    for milestone in recent_milestones:
        emoji = "📈" if milestone.direction == "UP" else "📉"
        time_str = milestone.timestamp.strftime("%H:%M:%S")
        lines.append(
            f"{emoji} {time_str} | {milestone.multiplier:.2f}x | "
            f"${milestone.price:.8f} | MC: ${milestone.market_cap:.0f}"
        )
    
    return "\n".join(lines)

# --- Sheet Operations (BATCHED) ---

def prepare_token_row_data(token: TokenMonitoring) -> List:
    """Prepare row data for a token"""
    profit_percent = 0.0
    if token.monitor_start_price > 0:
        profit_percent = ((token.current_price / token.monitor_start_price) - 1) * 100
    
    milestone_tracking = format_milestone_tracking(token)
    
    return [
        token.token_name,
        token.status,
        f"{token.monitor_start_price:.8f}",
        f"{token.current_price:.8f}",
        f"{token.monitor_start_market_cap:.0f}",
        f"{token.current_market_cap:.0f}",
        f"{token.highest_market_cap:.0f}",
        f"{token.current_multiplier:.2f}x",
        f"{token.lowest_multiplier:.2f}x",
        f"{profit_percent:.1f}%",
        milestone_tracking
    ]

def batch_update_tokens(tokens_to_update: List[TokenMonitoring]):
    """
    CRITICAL FIX: Batch update multiple tokens in a SINGLE API call
    This reduces API calls from N to 1, staying well under rate limits
    """
    if not tokens_to_update:
        return
    
    try:
        # Prepare batch update data
        data = []
        for token in tokens_to_update:
            row_data = prepare_token_row_data(token)
            range_str = f"{app_state.config.sheet_name}!B{token.row_index}:L{token.row_index}"
            data.append({
                'range': range_str,
                'values': [row_data]
            })
        
        # Single batchUpdate API call for all tokens
        body = {
            'valueInputOption': 'USER_ENTERED',
            'data': data
        }
        
        app_state.sheets_service.spreadsheets().values().batchUpdate(
            spreadsheetId=app_state.config.spreadsheet_id,
            body=body
        ).execute()
        
        logger.info(f"📝 Batch updated {len(tokens_to_update)} token(s) in sheet (1 API call)")
        
        # Mark tokens as updated
        for token in tokens_to_update:
            token.needs_update = False
            token.initial_write_done = True
    
    except HttpError as e:
        if hasattr(e, 'resp') and getattr(e.resp, 'status', None) == 429:
            logger.error("⚠️ Rate limit hit during batch update. Waiting 60s...")
            time.sleep(60)
        else:
            logger.error(f"Batch update failed: {e}")
    except Exception as e:
        logger.error(f"Batch update error: {e}")

# --- Token Scanning ---

def scan_for_new_tokens() -> List[Tuple[int, str]]:
    """Scan Column A for new token addresses"""
    try:
        range_str = f"{app_state.config.sheet_name}!B2:B"
        result = app_state.sheets_service.spreadsheets().values().get(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_str
        ).execute()
        
        values = result.get('values', [])
        new_tokens = []
        
        for i, row in enumerate(values):
            if not row or not row[0]:
                continue
            
            token_address = str(row[0]).strip()
            row_index = i + 2
            
            with app_state._lock:
                if token_address not in app_state.monitored_tokens:
                    new_tokens.append((row_index, token_address))
        
        return new_tokens
    
    except Exception as e:
        logger.error(f"Failed to scan for new tokens: {e}")
        return []

# --- Monitoring Logic ---

async def start_monitoring_token(row_index: int, token_address: str):
    """Initialize monitoring for a new token"""
    logger.info(f"🆕 Starting monitoring: Row {row_index} | {token_address[:8]}...")
    
    # Get initial data from DexScreener (single call is fine for initialization)
    token_name, price, market_cap = await get_dexscreener_data(token_address)
    
    if price == 0:
        logger.error(f"❌ Could not get price for {token_address[:8]}")
        return
    
    # Create token monitoring object
    token = TokenMonitoring(
        row_index=row_index,
        token_address=token_address,
        token_name=token_name,
        status="MONITORING",
        monitor_start_price=price,
        current_price=price,
        highest_price=price,
        lowest_price=price,
        monitor_start_market_cap=market_cap,
        current_market_cap=market_cap,
        highest_market_cap=market_cap,
        current_multiplier=1.0,
        highest_multiplier=1.0,
        lowest_multiplier=1.0,
        last_updated=datetime.now(),
        initial_write_done=False,
        needs_update=True  # Mark for initial write
    )
    
    # Add to monitored tokens
    with app_state._lock:
        app_state.monitored_tokens[token_address] = token
    
    logger.info(f"✅ Monitoring started: {token_name} | ${price:.8f} | MC: ${market_cap:.0f}")

async def update_token_prices_batch(tokens: List[TokenMonitoring]):
    """
    OPTIMIZED: Update multiple tokens in a single batch API call
    """
    if not tokens:
        return
    
    # Get all token addresses
    token_addresses = [t.token_address for t in tokens]
    
    # Fetch all prices in one batch call
    batch_results = await get_dexscreener_data_batch(token_addresses)
    
    # Process each token's results
    for token in tokens:
        try:
            token_name, price, market_cap = batch_results.get(
                token.token_address, 
                ("Unknown Token", 0.0, 0.0)
            )
            
            if price == 0:
                logger.warning(f"⚠️ Failed to get price for {token.token_name or token.token_address[:8]}")
                continue
            
            # Store previous multiplier
            old_multiplier = token.current_multiplier
            
            # Update current data
            token.current_price = price
            token.current_market_cap = market_cap
            token.last_updated = datetime.now()
            
            # Update name if we got a better one
            if token_name != "Unknown Token" and not token.token_name:
                token.token_name = token_name
            
            # Calculate current multiplier
            if token.monitor_start_price > 0:
                token.current_multiplier = token.current_price / token.monitor_start_price
            
            # Update highest/lowest
            if token.current_price > token.highest_price:
                token.highest_price = token.current_price
                token.highest_market_cap = market_cap
                token.highest_multiplier = token.current_multiplier
            
            if token.current_price < token.lowest_price:
                token.lowest_price = token.current_price
                token.lowest_multiplier = token.current_multiplier
            
            # Check for crossed milestones
            crossed_milestones = find_crossed_milestones(old_multiplier, token.current_multiplier)
            
            # Mark for update if milestones were crossed
            if crossed_milestones:
                for milestone_value, direction in crossed_milestones:
                    milestone = MultiplierMilestone(
                        timestamp=datetime.now(),
                        multiplier=milestone_value,
                        direction=direction,
                        price=token.current_price,
                        market_cap=token.current_market_cap
                    )
                    token.milestone_history.append(milestone)
                    
                    emoji = "📈" if direction == "UP" else "📉"
                    logger.info(
                        f"{emoji} {token.token_name or token.token_address[:8]} crossed {milestone_value:.2f}x {direction} | "
                        f"Current: {token.current_multiplier:.2f}x | Price: ${price:.8f}"
                    )
                
                token.needs_update = True  # Mark for batched update
            else:
                logger.debug(f"👀 {token.token_name or token.token_address[:8]}: {token.current_multiplier:.4f}x")
        
        except Exception as e:
            logger.error(f"Error processing {token.token_name or token.token_address[:8]}: {e}")

# --- Main Loop ---

async def monitoring_loop():
    """Main monitoring loop with batched updates"""
    logger.info("🔄 Starting monitoring loop...")
    
    while True:
        try:
            app_state.tick_counter += 1
            
            # Step 1: Scan for new tokens
            new_tokens = scan_for_new_tokens()
            
            if new_tokens:
                logger.info(f"📥 Found {len(new_tokens)} new token(s)")
                for row_index, token_address in new_tokens:
                    await start_monitoring_token(row_index, token_address)
            
            # Step 2: Update all monitored tokens IN ONE BATCH
            with app_state._lock:
                tokens_to_monitor = list(app_state.monitored_tokens.values())
            
            if tokens_to_monitor:
                logger.info(f"👀 Tick #{app_state.tick_counter}: Batch updating {len(tokens_to_monitor)} token(s)...")
                # SINGLE batch call for ALL tokens instead of N individual calls
                await update_token_prices_batch(tokens_to_monitor)
            
            # Step 3: BATCH UPDATE - Only tokens that need it
            with app_state._lock:
                # Tokens that crossed milestones or need initial write
                tokens_needing_update = [
                    t for t in app_state.monitored_tokens.values() 
                    if t.needs_update or not t.initial_write_done
                ]
                
                # Force update all tokens periodically (every N ticks)
                force_update = (app_state.tick_counter % app_state.config.batch_update_interval) == 0
                
                if force_update and tokens_to_monitor:
                    logger.info(f"🔄 Periodic update: Refreshing all {len(tokens_to_monitor)} tokens")
                    batch_update_tokens(tokens_to_monitor)
                elif tokens_needing_update:
                    logger.info(f"📝 Updating {len(tokens_needing_update)} token(s) with changes")
                    batch_update_tokens(tokens_needing_update)
            
            # Wait before next tick
            await asyncio.sleep(app_state.config.tick_interval_seconds)
        
        except Exception as e:
            logger.error(f"Error in monitoring loop: {e}", exc_info=True)
            await asyncio.sleep(5)

# --- Main Application ---

async def main_async():
    """Main async application"""
    logger.info("🚀 Starting Solana Token Monitor (DexScreener + Batched Updates)...")
    
    app_state.config = load_config()
    initialize_services()
    app_state.http_session = aiohttp.ClientSession()
    ensure_sheet_headers()
    
    # Test DexScreener
    logger.info("🧪 Testing DexScreener API...")
    try:
        test_token = "So11111111111111111111111111111111111111112"
        name, price, mc = await get_dexscreener_data(test_token)
        if price > 0:
            logger.info(f"✅ DexScreener API test passed: SOL = ${price:.2f}")
        else:
            logger.warning("⚠️ DexScreener API returned zero price for test token.")
    except Exception as e:
        logger.warning(f"DexScreener test failed: {e}")
    
    logger.info(f"🔄 Starting main loop (tick: {app_state.config.tick_interval_seconds}s)...")
    logger.info("📝 Using BATCHED updates to avoid rate limits")
    logger.info(f"🌐 DexScreener: Max {app_state.config.max_requests_per_minute} requests/min")
    logger.info(f"💡 With batching: Can monitor up to {app_state.config.max_requests_per_minute * 30} tokens!")
    
    try:
        await monitoring_loop()
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
