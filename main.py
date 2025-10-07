#!/usr/bin/env python3
"""
Enhanced Solana Token Monitoring Bot - DexScreener Real-Time Version
Monitors tokens from Column A and tracks multiplier milestones in spreadsheet
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

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(levelname)s - %(message)s',
    handlers=[
        logging.StreamHandler(sys.stdout),
        logging.FileHandler('/app/logs/dexscreener-monitor.log')
    ]
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

@dataclass
class Config:
    spreadsheet_id: str
    sheet_name: str
    tick_interval_seconds: float
    dexscreener_api_url: str
    rate_limit_delay: float

# --- Global State ---

class AppState:
    def __init__(self):
        self.sheets_service = None
        self.config: Optional[Config] = None
        self.http_session: Optional[aiohttp.ClientSession] = None
        self.monitored_tokens: Dict[str, TokenMonitoring] = {}
        self._lock = Lock()

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
    
    tick_interval_seconds = float(os.getenv("TICK_INTERVAL_SECONDS", "1"))  # 1 second default
    rate_limit_delay = float(os.getenv("RATE_LIMIT_MS", "100")) / 1000.0
    
    config = Config(
        spreadsheet_id=spreadsheet_id,
        sheet_name=sheet_name,
        tick_interval_seconds=tick_interval_seconds,
        dexscreener_api_url="https://api.dexscreener.com/latest/dex/tokens/",
        rate_limit_delay=rate_limit_delay
    )
    
    logger.info(f"✅ Configuration loaded: Tick interval: {tick_interval_seconds}s")
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
        "Start MC", "Current MC", "Highest MC", "Peak Multiplier", "Current Multiplier",
        "Lowest Multiplier", "Profit %", "Multiplier Tracking"
    ]]
    
    range_str = f"{app_state.config.sheet_name}!A1:M1"
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

async def get_dexscreener_data(token_address: str) -> Tuple[str, float, float]:
    """Fetch token data from DexScreener API"""
    url = f"{app_state.config.dexscreener_api_url}{token_address}"
    
    try:
        async with app_state.http_session.get(url, timeout=aiohttp.ClientTimeout(total=5)) as resp:
            if resp.status != 200:
                logger.warning(f"DexScreener returned status {resp.status} for {token_address[:8]}")
                return "Unknown Token", 0.0, 0.0
            
            data = await resp.json()
            pairs = data.get('pairs', [])
            
            if not pairs:
                logger.warning(f"No pairs found for {token_address[:8]}")
                return "Unknown Token", 0.0, 0.0
            
            # Get the most liquid pair (usually first one)
            pair = pairs[0]
            
            base_token = pair.get('baseToken', {})
            token_name = base_token.get('name') or base_token.get('symbol', 'Unknown Token')
            
            # Get price in USD
            price_usd = float(pair.get('priceUsd', 0))
            
            # Get market cap
            market_cap = float(pair.get('fdv', 0)) or float(pair.get('marketCap', 0))
            
            return token_name, price_usd, market_cap
    
    except asyncio.TimeoutError:
        logger.warning(f"Timeout fetching data for {token_address[:8]}")
        return "Unknown Token", 0.0, 0.0
    except Exception as e:
        logger.warning(f"DexScreener request failed for {token_address[:8]}: {e}")
        return "Unknown Token", 0.0, 0.0

# --- Milestone Tracking ---

def get_milestone_thresholds() -> List[float]:
    """Define multiplier milestones to track"""
    milestones = []
    
    # Fine-grained tracking below 1x (losses)
    milestones.extend([0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9])
    
    # Fine-grained tracking around breakeven
    milestones.extend([1.0, 1.1, 1.2, 1.3, 1.4, 1.5, 1.6, 1.7, 1.8, 1.9])
    
    # 0.2x increments up to 3x
    milestones.extend([2.0, 2.2, 2.4, 2.6, 2.8, 3.0])
    
    # 0.4x increments up to 10x
    milestones.extend([3.4, 3.8, 4.2, 4.6, 5.0, 5.4, 5.8, 6.2, 6.6, 7.0, 7.4, 7.8, 8.2, 8.6, 9.0, 9.4, 9.8, 10.0])
    
    # 1x increments up to 20x
    milestones.extend([11.0, 12.0, 13.0, 14.0, 15.0, 16.0, 17.0, 18.0, 19.0, 20.0])
    
    # 5x increments up to 50x
    milestones.extend([25.0, 30.0, 35.0, 40.0, 45.0, 50.0])
    
    # 10x increments up to 100x
    milestones.extend([60.0, 70.0, 80.0, 90.0, 100.0])
    
    # 50x increments beyond
    milestones.extend([150.0, 200.0, 250.0, 300.0, 400.0, 500.0, 1000.0])
    
    return milestones

def find_crossed_milestones(old_multiplier: float, new_multiplier: float) -> List[Tuple[float, str]]:
    """Find all milestones crossed between old and new multiplier"""
    milestones = get_milestone_thresholds()
    crossed = []
    
    if new_multiplier > old_multiplier:
        # Moving up
        for milestone in milestones:
            if old_multiplier < milestone <= new_multiplier:
                crossed.append((milestone, "UP"))
    else:
        # Moving down
        for milestone in reversed(milestones):
            if new_multiplier < milestone <= old_multiplier:
                crossed.append((milestone, "DOWN"))
    
    return crossed

def format_milestone_tracking(token: TokenMonitoring) -> str:
    """Format milestone history for spreadsheet display"""
    if not token.milestone_history:
        return ""
    
    # Show last 20 milestones to avoid cell size limits
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

# --- Sheet Operations ---

def scan_for_new_tokens() -> List[Tuple[int, str]]:
    """Scan Column A for new token addresses"""
    try:
        range_str = f"{app_state.config.sheet_name}!A2:A"
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
            
            # Check if we're already monitoring this token
            with app_state._lock:
                if token_address not in app_state.monitored_tokens:
                    new_tokens.append((row_index, token_address))
        
        return new_tokens
    
    except Exception as e:
        logger.error(f"Failed to scan for new tokens: {e}")
        return []

def update_token_row(token: TokenMonitoring):
    """Update a single token's row in the sheet"""
    try:
        profit_percent = 0.0
        if token.monitor_start_price > 0:
            profit_percent = ((token.current_price / token.monitor_start_price) - 1) * 100
        
        milestone_tracking = format_milestone_tracking(token)
        
        values = [[
            token.token_name,
            token.status,
            f"{token.monitor_start_price:.8f}",
            f"{token.current_price:.8f}",
            f"{token.monitor_start_market_cap:.0f}",
            f"{token.current_market_cap:.0f}",
            f"{token.highest_market_cap:.0f}",
            f"{token.highest_multiplier:.2f}x",
            f"{token.current_multiplier:.2f}x",
            f"{token.lowest_multiplier:.2f}x",
            f"{profit_percent:.1f}%",
            milestone_tracking
        ]]
        
        range_str = f"{app_state.config.sheet_name}!B{token.row_index}:M{token.row_index}"
        body = {'values': values}
        
        app_state.sheets_service.spreadsheets().values().update(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_str,
            valueInputOption='USER_ENTERED',
            body=body
        ).execute()
    
    except Exception as e:
        logger.warning(f"Failed to update row {token.row_index}: {e}")

def update_status_only(row_index: int, status: str):
    """Quick status update"""
    try:
        range_str = f"{app_state.config.sheet_name}!C{row_index}"
        body = {'values': [[status]]}
        app_state.sheets_service.spreadsheets().values().update(
            spreadsheetId=app_state.config.spreadsheet_id,
            range=range_str,
            valueInputOption='RAW',
            body=body
        ).execute()
    except Exception as e:
        logger.warning(f"Failed to update status: {e}")

# --- Monitoring Logic ---

async def start_monitoring_token(row_index: int, token_address: str):
    """Initialize monitoring for a new token"""
    logger.info(f"🆕 Starting monitoring: Row {row_index} | {token_address[:8]}...")
    
    update_status_only(row_index, "INITIALIZING")
    
    # Get initial data from DexScreener
    await asyncio.sleep(app_state.config.rate_limit_delay)
    token_name, price, market_cap = await get_dexscreener_data(token_address)
    
    if price == 0:
        logger.error(f"❌ Could not get price for {token_address[:8]}")
        update_status_only(row_index, "FAILED")
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
        last_updated=datetime.now()
    )
    
    # Add to monitored tokens
    with app_state._lock:
        app_state.monitored_tokens[token_address] = token
    
    # Update sheet
    update_token_row(token)
    
    logger.info(f"✅ Monitoring started: {token_name} | ${price:.8f} | MC: ${market_cap:.0f}")

async def update_token_price(token: TokenMonitoring):
    """Update token price and track milestones"""
    try:
        # Get current data from DexScreener
        await asyncio.sleep(app_state.config.rate_limit_delay)
        token_name, price, market_cap = await get_dexscreener_data(token.token_address)
        
        if price == 0:
            logger.warning(f"⚠️ Failed to get price for {token.token_name}")
            return
        
        # Store previous multiplier
        old_multiplier = token.current_multiplier
        
        # Update current data
        token.current_price = price
        token.current_market_cap = market_cap
        token.last_updated = datetime.now()
        
        # Calculate current multiplier
        if token.monitor_start_price > 0:
            token.current_multiplier = token.current_price / token.monitor_start_price
        
        # Update highest price and multiplier
        if token.current_price > token.highest_price:
            token.highest_price = token.current_price
            token.highest_market_cap = market_cap
            token.highest_multiplier = token.current_multiplier
        
        # Update lowest price and multiplier
        if token.current_price < token.lowest_price:
            token.lowest_price = token.current_price
            token.lowest_multiplier = token.current_multiplier
        
        # Check for crossed milestones
        crossed_milestones = find_crossed_milestones(old_multiplier, token.current_multiplier)
        
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
                f"{emoji} {token.token_name} crossed {milestone_value:.2f}x {direction} | "
                f"Current: {token.current_multiplier:.2f}x | Price: ${price:.8f}"
            )
        
        # Update sheet
        update_token_row(token)
    
    except Exception as e:
        logger.error(f"Error updating {token.token_name}: {e}")

# --- Main Loop ---

async def monitoring_loop():
    """Main monitoring loop"""
    logger.info("🔄 Starting monitoring loop...")
    
    while True:
        try:
            # Step 1: Scan for new tokens in Column A
            new_tokens = scan_for_new_tokens()
            
            if new_tokens:
                logger.info(f"📥 Found {len(new_tokens)} new token(s)")
                for row_index, token_address in new_tokens:
                    await start_monitoring_token(row_index, token_address)
            
            # Step 2: Update all monitored tokens
            with app_state._lock:
                tokens_to_update = list(app_state.monitored_tokens.values())
            
            if tokens_to_update:
                logger.info(f"👀 Updating {len(tokens_to_update)} token(s)...")
                tasks = [update_token_price(token) for token in tokens_to_update]
                await asyncio.gather(*tasks, return_exceptions=True)
            
            # Wait before next tick
            await asyncio.sleep(app_state.config.tick_interval_seconds)
        
        except Exception as e:
            logger.error(f"Error in monitoring loop: {e}")
            await asyncio.sleep(5)

# --- Main Application ---

async def main_async():
    """Main async application"""
    logger.info("🚀 Starting Solana Token Monitor (DexScreener Version)...")
    
    # Load configuration
    app_state.config = load_config()
    
    # Initialize services
    initialize_services()
    
    # Create HTTP session
    app_state.http_session = aiohttp.ClientSession()
    
    # Ensure sheet headers
    ensure_sheet_headers()
    
    # Test DexScreener connectivity
    logger.info("🧪 Testing DexScreener API...")
    try:
        test_token = "So11111111111111111111111111111111111111112"  # SOL
        name, price, mc = await get_dexscreener_data(test_token)
        if price > 0:
            logger.info(f"✅ DexScreener API test passed: SOL = ${price:.2f}")
    except Exception as e:
        logger.warning(f"DexScreener test failed: {e}")
    
    # Start monitoring loop
    logger.info(f"🔄 Starting main loop (tick: {app_state.config.tick_interval_seconds}s)...")
    
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