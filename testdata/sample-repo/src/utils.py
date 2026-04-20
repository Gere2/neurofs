"""Utility functions shared across the application."""

import hashlib
import json
import logging
import os
from datetime import datetime
from typing import Any, Dict, List, Optional

logger = logging.getLogger(__name__)


def slugify(text: str) -> str:
    """Convert a string to a URL-safe slug."""
    import re
    text = text.lower().strip()
    text = re.sub(r'[^\w\s-]', '', text)
    text = re.sub(r'[\s_-]+', '-', text)
    return re.sub(r'^-+|-+$', '', text)


def compute_checksum(data: bytes) -> str:
    """Return the SHA-256 hex digest of data."""
    return hashlib.sha256(data).hexdigest()


def paginate(items: List[Any], page: int, per_page: int) -> Dict[str, Any]:
    """Paginate a list of items."""
    start = (page - 1) * per_page
    end = start + per_page
    return {
        "items": items[start:end],
        "page": page,
        "per_page": per_page,
        "total": len(items),
        "pages": (len(items) + per_page - 1) // per_page,
    }


def load_json_file(path: str) -> Optional[Dict]:
    """Load a JSON file safely; returns None on error."""
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        logger.error("Failed to load %s: %s", path, e)
        return None


class EventBus:
    """Minimal synchronous event bus for decoupled component communication."""

    def __init__(self):
        self._handlers: Dict[str, List] = {}

    def subscribe(self, event: str, handler) -> None:
        self._handlers.setdefault(event, []).append(handler)

    def publish(self, event: str, payload: Any = None) -> None:
        for handler in self._handlers.get(event, []):
            try:
                handler(payload)
            except Exception as e:
                logger.error("Handler error for %s: %s", event, e)
