# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project: magento2-cart-graphql-go

Go drop-in replacement for Magento 2's cart/checkout GraphQL. Write-heavy, stateful operations with tax calculation and order placement. Verified field-by-field against Magento 2.4.8 PHP.

## Current State

**Phase 1: Complete (9/9 tasks).** Full guest checkout flow verified field-by-field against Magento PHP. All 8 tests passing (6 integration + 2 comparison). Implementation audited and synced against actual Magento DB schema (PR #28).

### What works (verified against Magento PHP)
- Cart creation (guest + customer), masked ID generation
- Add simple products (SKU lookup, status/stock validation, price from index)
- Update quantity, remove items, duplicate SKU merging
- Shipping addresses with region resolution (stores full name from directory_country_region)
- Billing addresses (explicit or same_as_shipping)
- Shipping methods: flatrate (per-item, default $5×qty), tablerate (website-scoped, price column)
- Payment methods: checkmo, banktransfer, cashondelivery, purchaseorder, free (from config)
- Guest email on cart
- Tax: US state-level, product tax class via eav_attribute.default_value fallback
- Totals recalculation after every cart modification
- Place order: full transactional flow with correct sales_order fields, address ID backfill, grid data

### Known gaps (documented as GitHub issues)
- PlaceOrderOutput schema uses simplified `PlacedOrder{number}` instead of Magento's `CustomerOrder` type with `errors: [PlaceOrderError!]!` field — Magento clients expecting structured errors in data payload will see GraphQL-level errors instead
- Discount amounts hardcoded to 0 on placed orders (coupon monetary effect not propagated to sales_order)
- `product_options` JSON not stored on sales_order_item
- `remote_ip` not captured on sales_order
- `email_sent` not set (Go doesn't send order emails)

## Build & Test

```bash
GOTOOLCHAIN=auto go build ./cmd/server/
GOTOOLCHAIN=auto go vet ./...
GOTOOLCHAIN=auto go run github.com/99designs/gqlgen generate

# Run all tests (requires MySQL + Magento at :8080 for comparison tests)
GOTOOLCHAIN=auto go test ./tests/ -v -timeout 300s -count=1

# Run only integration tests (no Magento needed)
GOTOOLCHAIN=auto go test ./tests/ -run "^Test[^C]" -v -timeout 60s -count=1

# Run server (port 8084)
MAGENTO_CRYPT_KEY="<key>" DB_USER=magento_go DB_PASSWORD=magento_go DB_NAME=magento248 GOTOOLCHAIN=auto go run ./cmd/server/
```

## Architecture

- **ConfigProvider** from day 1 — no raw `core_config_data` queries anywhere
- **Cart ID masking** — all external operations use 32-char masked IDs from `quote_id_mask`
- **Totals recalculation** — runs after every cart modification (add/remove/update/address/shipping)
- **Tax** — looks up `tax_calculation_rate` → `tax_calculation` → `tax_calculation_rule` matching country/region + product/customer tax class. Falls back to `eav_attribute.default_value` for product tax class.
- **Region resolution** — when `region_id` is provided, stores full name from `directory_country_region` (e.g., "Texas" not "TX")
- **Shipping** — tablerate uses `price` column (not `cost`), scoped by `website_id`; flatrate defaults active with per-item pricing

## Key Database Tables

| Table | R/W | Purpose |
|-------|-----|---------|
| `quote` | R/W | Cart entity, totals |
| `quote_item` | R/W | Line items |
| `quote_address` | R/W | Billing + shipping addresses |
| `quote_payment` | R/W | Selected payment |
| `quote_id_mask` | R/W | Masked ID mapping |
| `shipping_tablerate` | R | Tablerate shipping lookup (price column, website-scoped) |
| `tax_calculation_rate` | R | Tax rates by country/region |
| `tax_calculation` | R | Rate → rule → product/customer class |
| `catalog_product_entity` | R | Product lookup for add-to-cart |
| `catalog_product_index_price` | R | Product pricing |
| `cataloginventory_stock_item` | R | Stock validation |
| `eav_attribute` | R | Attribute metadata + default values |
| `directory_country_region` | R | Region code/name resolution |
| `sales_order` | W | Order creation |
| `sales_order_item` | W | Order line items |
| `sales_order_address` | W | Order addresses |
| `sales_order_payment` | W | Order payment |
| `sales_order_grid` | W | Admin grid data |
| `inventory_reservation` | W | Stock reservation (negative qty) |
| `sequence_order_1` | W | Order increment ID reservation |

## Tax Scope

### What works
- US state-level tax (region_id based)
- Product tax class matching via `eav_attribute.default_value` fallback
- Customer tax class (default: Retail Customer, class 3)
- tax_calculation_rate → tax_calculation → tax_calculation_rule join

### What doesn't work yet
- EU VAT (country-level, no region) — #19
- Tax-inclusive pricing (price_includes_tax config) — #20
- Tax on shipping — #21
- Compound/stacked tax rules — #22
- FPT/WEEE tax

## Product Types

### Supported
- Simple products: add, update qty, remove, price, stock check

### Not yet
- Configurable: need selected_options decoding, parent+child quote_items — #11
- Bundle: need bundle option parsing, dynamic pricing — #12
- Virtual/Downloadable: need is_virtual cart detection, no shipping — #23
- Grouped: add children as individual simple items — #24

## Lessons Learned

### From this project
- DESCRIBE every table before writing SQL — `cost` vs `price` column burned us on tablerate
- `eav_attribute.default_value` matters — products without explicit EAV rows use it
- Magento stores full region names ("Texas"), not codes ("TX") — resolve via `directory_country_region`
- `sales_order` requires address ID backfill after inserting `sales_order_address`
- `store_to_base_rate` and `store_to_order_rate` are 0 in Magento (not 1)
- Grid address format: `street,city,region,postcode` — no name/country, comma without spaces
- Flatrate shipping is per-item by default (`type=I`, price × qty)
- PlaceOrder error messages must NOT be prefixed with "Unable to place order:"

### From catalog + customer projects
- Use ConfigProvider for all core_config_data reads
- Never hardcode attribute IDs — use subqueries against eav_attribute
- Always `redis-cli FLUSHALL` when testing after code changes
- Error messages must match Magento exactly (capitalized, with period)
- One PR per ticket, branch per feature
