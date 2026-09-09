"""Pure, explicit protocol-2 requests for the isolated whole-trade demos."""


def integer(value, maximum):
    # Accept protobuf JSON decimal strings without a float round-trip.
    if isinstance(value, bool) or not isinstance(value, (int, str)):
        raise ValueError("Expected an exact nonnegative integer")
    if isinstance(value, str) and (not value or not value.isascii() or not value.isdecimal()):
        raise ValueError("Expected an exact decimal integer")
    result = int(value)
    if not 0 <= result <= maximum:
        raise ValueError("Integer outside the protocol field range")
    return result


def whole_offer(sell_amount=1_000_000, buy_amount=2_000_000, tower_bps=0):
    # These are fixture-reviewed limits, not limits learned from a public offer.
    return {"sell": "btc", "sell_amount": sell_amount, "buy_amount": buy_amount,
            "fill_mode": "whole", "min_fill": sell_amount, "max_fill": sell_amount,
            "funding_fee": 2000, "tower_bps": tower_bps,
            "fee_budgets": {"btc": 50_000, "blake": 50_000},
            "bounty_budgets": {"btc": (sell_amount * tower_bps + 9999) // 10000,
                               "blake": (buy_amount * tower_bps + 9999) // 10000}}


def matching_parent(created, candidate):
    return candidate.get("id") == created.get("id") and candidate.get("maker") == created.get("maker")


def whole_take(created, observed):
    # Do not enlarge the intended trade or take a foreign maker's reused ID.
    for key in ("id", "maker", "network", "sell", "fill_mode"):
        if not created.get(key) or created.get(key) != observed.get(key):
            raise ValueError("Delivered parent differs from the created whole order")
    if created["network"] != "regtest" or observed["fill_mode"] != "whole" or observed.get("status") != "open":
        raise ValueError("Expected an open whole regtest parent")
    if integer(created.get("version"), 2**31-1) != 1 or integer(observed.get("version"), 2**31-1) != 1:
        raise ValueError("Expected protocol 1; preserve incompatible development data")
    for key in ("sell_amount", "buy_amount", "min_fill", "max_fill"):
        if integer(created.get(key), 2**63-1) != integer(observed.get(key), 2**63-1):
            raise ValueError("Delivered parent changed its immutable whole terms")
    quantity = integer(created["sell_amount"], 2**63-1)
    if quantity <= 0 or any(integer(observed[key], 2**63-1) != quantity for key in ("min_fill", "max_fill", "available")):
        raise ValueError("The exact whole quantity is no longer available")
    revision = integer(observed.get("revision"), 2**64-1)
    if not revision:
        raise ValueError("An explicit parent revision is required")
    return {"maker": observed["maker"], "id": observed["id"], "quantity": quantity,
            "parent_revision": revision, "funding_fee": 2000}
