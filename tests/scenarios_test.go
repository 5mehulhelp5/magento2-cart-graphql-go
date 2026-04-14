package tests

// scenarios_test.go — Order placement scenarios for admin smoke-testing
//
// Each TestScenarios sub-test places a real order via the Go GraphQL service,
// verifies cart totals and discount amounts, then writes all resulting order
// increment IDs to magento2-admin-tests/fixtures/orders.json so the Playwright
// admin suite can run against them.
//
// Run:
//   GOTOOLCHAIN=auto go test ./tests/ -run "^TestScenarios$" -v -timeout 120s -count=1
//
// The fixture file is written only when ALL sub-tests complete (pass or skip).
// Any sub-test failure still writes its zero-value so the admin fixture is
// always left in a consistent state.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// ─── Fixture output ──────────────────────────────────────────────────────────

type scenarioFixture struct {
	Comment                 string            `json:"_comment"`
	AllOrders               []string          `json:"allOrders"`
	OrderForInvoiceShipment string            `json:"orderForInvoiceShipment"`
	OrderForCreditMemo      string            `json:"orderForCreditMemo"`
	Scenarios               map[string]string `json:"scenarios"`
}

// ─── Checkout helpers ─────────────────────────────────────────────────────────

// cartItem describes one line in addProductsToCart.
type cartItem struct {
	SKU             string
	Quantity        int
	SelectedOptions []string // base64 UIDs for configurable options
}

// mustCreateCart creates a guest cart and returns its masked ID.
func mustCreateCart(t *testing.T) string {
	t.Helper()
	resp := doQuery(t, `mutation { createEmptyCart }`, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("createEmptyCart: %s", resp.Errors[0].Message)
	}
	var d struct {
		CreateEmptyCart string `json:"createEmptyCart"`
	}
	json.Unmarshal(resp.Data, &d)
	if len(d.CreateEmptyCart) != 32 {
		t.Fatalf("createEmptyCart returned bad ID: %q", d.CreateEmptyCart)
	}
	return d.CreateEmptyCart
}

// mustAddItems calls addProductsToCart for all supplied items and asserts no errors.
func mustAddItems(t *testing.T, cartID string, items []cartItem) {
	t.Helper()
	for _, item := range items {
		var optStr string
		if len(item.SelectedOptions) > 0 {
			for i, o := range item.SelectedOptions {
				if i > 0 {
					optStr += ", "
				}
				optStr += fmt.Sprintf("%q", o)
			}
			optStr = "selected_options: [" + optStr + "]"
		}
		q := fmt.Sprintf(`mutation {
			addProductsToCart(cartId: %q, cartItems: [{sku: %q, quantity: %d %s}]) {
				cart { total_quantity }
				user_errors { code message }
			}
		}`, cartID, item.SKU, item.Quantity, optStr)
		resp := doQuery(t, q, "")
		if len(resp.Errors) > 0 {
			t.Fatalf("addProductsToCart sku=%s: %s", item.SKU, resp.Errors[0].Message)
		}
		var d struct {
			AddProductsToCart struct {
				UserErrors []struct{ Code, Message string } `json:"user_errors"`
			} `json:"addProductsToCart"`
		}
		json.Unmarshal(resp.Data, &d)
		if len(d.AddProductsToCart.UserErrors) > 0 {
			t.Fatalf("addProductsToCart sku=%s user_error: %s", item.SKU, d.AddProductsToCart.UserErrors[0].Message)
		}
	}
}

// maybeApplyCoupon applies coupon if non-empty. Returns the applied coupon code
// from the response for logging.
func maybeApplyCoupon(t *testing.T, cartID, coupon string) {
	t.Helper()
	if coupon == "" {
		return
	}
	q := fmt.Sprintf(`mutation {
		applyCouponToCart(input: { cart_id: %q, coupon_code: %q }) {
			cart { applied_coupons { code } }
		}
	}`, cartID, coupon)
	resp := doQuery(t, q, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("applyCouponToCart code=%s: %s", coupon, resp.Errors[0].Message)
	}
	var d struct {
		ApplyCouponToCart struct {
			Cart struct {
				AppliedCoupons []struct{ Code string } `json:"applied_coupons"`
			} `json:"cart"`
		} `json:"applyCouponToCart"`
	}
	json.Unmarshal(resp.Data, &d)
	if len(d.ApplyCouponToCart.Cart.AppliedCoupons) == 0 {
		t.Fatalf("applyCouponToCart: coupon %s not applied", coupon)
	}
	t.Logf("  coupon applied: %s", d.ApplyCouponToCart.Cart.AppliedCoupons[0].Code)
}

// mustSetAddresses sets the Texas address on shipping and billing.
func mustSetAddresses(t *testing.T, cartID string) {
	t.Helper()
	addr := `{
		firstname: "Scenario"
		lastname:  "Test"
		street:    ["123 Main St"]
		city:      "Austin"
		region:    "TX"
		region_id: 57
		postcode:  "78701"
		country_code: "US"
		telephone: "5125551234"
	}`
	// Shipping address
	q := fmt.Sprintf(`mutation {
		setShippingAddressesOnCart(input: {
			cart_id: %q
			shipping_addresses: [{ address: %s }]
		}) { cart { id } }
	}`, cartID, addr)
	resp := doQuery(t, q, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("setShippingAddressesOnCart: %s", resp.Errors[0].Message)
	}
	// Billing address (same)
	q = fmt.Sprintf(`mutation {
		setBillingAddressOnCart(input: {
			cart_id: %q
			billing_address: { address: %s }
		}) { cart { id } }
	}`, cartID, addr)
	resp = doQuery(t, q, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("setBillingAddressOnCart: %s", resp.Errors[0].Message)
	}
}

// mustSetShipping selects flatrate shipping.
func mustSetShipping(t *testing.T, cartID string) {
	t.Helper()
	q := fmt.Sprintf(`mutation {
		setShippingMethodsOnCart(input: {
			cart_id: %q
			shipping_methods: [{ carrier_code: "flatrate", method_code: "flatrate" }]
		}) { cart { id } }
	}`, cartID)
	resp := doQuery(t, q, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("setShippingMethodsOnCart: %s", resp.Errors[0].Message)
	}
}

// mustSetPaymentAndEmail sets checkmo payment and guest email.
func mustSetPaymentAndEmail(t *testing.T, cartID string) {
	t.Helper()
	q := fmt.Sprintf(`mutation {
		setPaymentMethodOnCart(input: {
			cart_id: %q
			payment_method: { code: "checkmo" }
		}) { cart { id } }
	}`, cartID)
	resp := doQuery(t, q, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("setPaymentMethodOnCart: %s", resp.Errors[0].Message)
	}
	q = fmt.Sprintf(`mutation {
		setGuestEmailOnCart(input: {
			cart_id: %q
			email: "scenario-test@example.com"
		}) { cart { email } }
	}`, cartID)
	resp = doQuery(t, q, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("setGuestEmailOnCart: %s", resp.Errors[0].Message)
	}
}

// cartTotals fetches cart prices for assertion.
type cartTotals struct {
	SubtotalExcl         float64
	SubtotalWithDiscount float64
	GrandTotal           float64
	DiscountAmount       float64 // sum of all discounts
}

func getCartTotals(t *testing.T, cartID string) cartTotals {
	t.Helper()
	q := fmt.Sprintf(`{
		cart(cart_id: %q) {
			prices {
				subtotal_excluding_tax         { value }
				subtotal_with_discount_excluding_tax { value }
				grand_total                    { value }
				discounts { amount { value } }
			}
		}
	}`, cartID)
	resp := doQuery(t, q, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("cart prices query: %s", resp.Errors[0].Message)
	}
	var d struct {
		Cart struct {
			Prices struct {
				SubtotalExcludingTax struct{ Value float64 } `json:"subtotal_excluding_tax"`
				SubtotalWithDiscount struct{ Value float64 } `json:"subtotal_with_discount_excluding_tax"`
				GrandTotal           struct{ Value float64 } `json:"grand_total"`
				Discounts            []struct {
					Amount struct{ Value float64 } `json:"amount"`
				} `json:"discounts"`
			} `json:"prices"`
		} `json:"cart"`
	}
	if err := json.Unmarshal(resp.Data, &d); err != nil {
		t.Fatalf("parse cart prices: %v", err)
	}
	ct := cartTotals{
		SubtotalExcl:         d.Cart.Prices.SubtotalExcludingTax.Value,
		SubtotalWithDiscount: d.Cart.Prices.SubtotalWithDiscount.Value,
		GrandTotal:           d.Cart.Prices.GrandTotal.Value,
	}
	for _, disc := range d.Cart.Prices.Discounts {
		ct.DiscountAmount += disc.Amount.Value
	}
	return ct
}

// mustPlaceOrder submits the cart and returns the order increment_id.
func mustPlaceOrder(t *testing.T, cartID string) string {
	t.Helper()
	q := fmt.Sprintf(`mutation {
		placeOrder(input: { cart_id: %q }) {
			errors { code message }
			orderV2 { number }
		}
	}`, cartID)
	resp := doQuery(t, q, "")
	if len(resp.Errors) > 0 {
		t.Fatalf("placeOrder: %s", resp.Errors[0].Message)
	}
	var d struct {
		PlaceOrder struct {
			Errors []struct{ Code, Message string } `json:"errors"`
			OrderV2 struct {
				Number string `json:"number"`
			} `json:"orderV2"`
		} `json:"placeOrder"`
	}
	json.Unmarshal(resp.Data, &d)
	if len(d.PlaceOrder.Errors) > 0 {
		t.Fatalf("placeOrder errors: %v", d.PlaceOrder.Errors)
	}
	if d.PlaceOrder.OrderV2.Number == "" {
		t.Fatal("placeOrder returned empty order number")
	}
	return d.PlaceOrder.OrderV2.Number
}

// approxEqual checks that |a-b| <= tolerance (for floating-point money).
func approxEqual(t *testing.T, label string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %.4f, want %.4f (tolerance %.4f)", label, got, want, tol)
	}
}

// ─── Coupon setup ─────────────────────────────────────────────────────────────

// ensureTestCoupons creates FIXED5 and SAVE20 rules in the DB if they do not
// already exist. Safe to call multiple times.
func ensureTestCoupons(t *testing.T) {
	t.Helper()

	type couponSpec struct {
		code           string
		name           string
		simpleAction   string
		discountAmount float64
	}
	specs := []couponSpec{
		{"FIXED5", "Test FIXED5 — $5 off per unit (all products)", "by_fixed", 5.0},
		{"SAVE20", "Test SAVE20 — $20 off cart total (all products)", "cart_fixed", 20.0},
	}

	emptyConds := `{"type":"Magento\\\\SalesRule\\\\Model\\\\Rule\\\\Condition\\\\Combine","attribute":null,"operator":null,"value":"1","is_value_processed":null,"aggregator":"all","conditions":[]}`

	for _, spec := range specs {
		// Check if coupon already exists
		var existingID int
		err := testDB.QueryRowContext(context.Background(),
			"SELECT coupon_id FROM salesrule_coupon WHERE code = ?", spec.code,
		).Scan(&existingID)
		if err == nil {
			t.Logf("  coupon %s already exists (coupon_id=%d)", spec.code, existingID)
			continue
		}
		if err != sql.ErrNoRows {
			t.Fatalf("checking coupon %s: %v", spec.code, err)
		}

		// Insert the rule
		res, err := testDB.ExecContext(context.Background(), `
			INSERT INTO salesrule
			  (name, from_date, to_date, uses_per_customer, is_active,
			   conditions_serialized, actions_serialized,
			   stop_rules_processing, is_advanced, product_ids,
			   sort_order, simple_action, discount_amount, discount_qty,
			   discount_step, apply_to_shipping, times_used, is_rss,
			   coupon_type, use_auto_generation, uses_per_coupon, simple_free_shipping)
			VALUES (?, NULL, NULL, 0, 1, ?, ?, 0, 1, NULL,
			        0, ?, ?, NULL, 0, 0, 0, 0, 2, 0, 0, 0)`,
			spec.name, emptyConds, emptyConds, spec.simpleAction, spec.discountAmount,
		)
		if err != nil {
			t.Fatalf("insert salesrule %s: %v", spec.code, err)
		}
		ruleID, _ := res.LastInsertId()

		// Insert coupon code
		_, err = testDB.ExecContext(context.Background(), `
			INSERT INTO salesrule_coupon
			  (rule_id, code, is_primary, usage_limit, usage_per_customer,
			   times_used, created_at, type)
			VALUES (?, ?, 1, NULL, 0, 0, ?, 0)`,
			ruleID, spec.code, time.Now(),
		)
		if err != nil {
			t.Fatalf("insert salesrule_coupon %s: %v", spec.code, err)
		}

		// Website scope
		_, err = testDB.ExecContext(context.Background(),
			"INSERT INTO salesrule_website (rule_id, website_id) VALUES (?, 1)", ruleID)
		if err != nil {
			t.Fatalf("insert salesrule_website %s: %v", spec.code, err)
		}

		// Customer groups (0=guest, 1=General, 2=Wholesale, 3=Retailer)
		for _, grp := range []int{0, 1, 2, 3} {
			testDB.ExecContext(context.Background(),
				"INSERT INTO salesrule_customer_group (rule_id, customer_group_id) VALUES (?, ?)",
				ruleID, grp)
		}

		t.Logf("  created coupon %s (rule_id=%d)", spec.code, ruleID)
	}
}

// ─── Scenario functions ───────────────────────────────────────────────────────

// S01: Single simple product, no discount — baseline
// Used for: Invoice + Shipment in admin
func runS01(t *testing.T) string {
	cartID := mustCreateCart(t)
	mustAddItems(t, cartID, []cartItem{
		{SKU: "24-MB01", Quantity: 2},
	})
	mustSetAddresses(t, cartID)
	mustSetShipping(t, cartID)
	mustSetPaymentAndEmail(t, cartID)

	ct := getCartTotals(t, cartID)
	approxEqual(t, "S01 subtotal", ct.SubtotalExcl, 68.00, 0.02)
	approxEqual(t, "S01 discount", ct.DiscountAmount, 0.00, 0.02)
	if ct.GrandTotal <= 0 {
		t.Errorf("S01 grand_total must be positive, got %.2f", ct.GrandTotal)
	}
	t.Logf("  subtotal=%.2f discount=%.2f grand_total=%.2f", ct.SubtotalExcl, ct.DiscountAmount, ct.GrandTotal)

	id := mustPlaceOrder(t, cartID)
	t.Logf("  order placed: %s", id)
	return id
}

// S02: Multiple simple products, no discount
// Used for: Invoice + Credit Memo in admin
func runS02(t *testing.T) string {
	cartID := mustCreateCart(t)
	mustAddItems(t, cartID, []cartItem{
		{SKU: "24-MB01", Quantity: 1},
		{SKU: "24-WB01", Quantity: 1},
		{SKU: "24-UG06", Quantity: 3},
	})
	mustSetAddresses(t, cartID)
	mustSetShipping(t, cartID)
	mustSetPaymentAndEmail(t, cartID)

	ct := getCartTotals(t, cartID)
	approxEqual(t, "S02 subtotal", ct.SubtotalExcl, 87.00, 0.02)
	approxEqual(t, "S02 discount", ct.DiscountAmount, 0.00, 0.02)
	t.Logf("  subtotal=%.2f discount=%.2f grand_total=%.2f", ct.SubtotalExcl, ct.DiscountAmount, ct.GrandTotal)

	id := mustPlaceOrder(t, cartID)
	t.Logf("  order placed: %s", id)
	return id
}

// S03: Configurable product (MH01 Black/XS), no discount
// Tests that ConfigurableCartItem is created correctly
func runS03(t *testing.T) string {
	// MH01 Chaz Kangeroo Hoodie
	// color=Black (attribute_id=93, option_id=49) → UID: configurable/93/49  → Y29uZmlndXJhYmxlLzkzLzQ5
	// size=XS    (attribute_id=142, option_id=166) → UID: configurable/142/166 → Y29uZmlndXJhYmxlLzE0Mi8xNjY=
	cartID := mustCreateCart(t)
	mustAddItems(t, cartID, []cartItem{
		{
			SKU:      "MH01",
			Quantity: 2,
			SelectedOptions: []string{
				"Y29uZmlndXJhYmxlLzkzLzQ5",   // color: Black
				"Y29uZmlndXJhYmxlLzE0Mi8xNjY=", // size: XS
			},
		},
	})
	mustSetAddresses(t, cartID)
	mustSetShipping(t, cartID)
	mustSetPaymentAndEmail(t, cartID)

	ct := getCartTotals(t, cartID)
	approxEqual(t, "S03 subtotal", ct.SubtotalExcl, 104.00, 0.02)
	approxEqual(t, "S03 discount", ct.DiscountAmount, 0.00, 0.02)
	t.Logf("  subtotal=%.2f discount=%.2f grand_total=%.2f", ct.SubtotalExcl, ct.DiscountAmount, ct.GrandTotal)

	id := mustPlaceOrder(t, cartID)
	t.Logf("  order placed: %s", id)
	return id
}

// S04: Coupon H20 — by_percent 70% off SKU 24-UG06
func runS04(t *testing.T) string {
	cartID := mustCreateCart(t)
	mustAddItems(t, cartID, []cartItem{
		{SKU: "24-UG06", Quantity: 3},
	})
	maybeApplyCoupon(t, cartID, "H20")
	mustSetAddresses(t, cartID)
	mustSetShipping(t, cartID)
	mustSetPaymentAndEmail(t, cartID)

	ct := getCartTotals(t, cartID)
	approxEqual(t, "S04 subtotal", ct.SubtotalExcl, 21.00, 0.02)
	// 70% of $21 = $14.70
	approxEqual(t, "S04 discount", ct.DiscountAmount, 14.70, 0.02)
	approxEqual(t, "S04 discounted subtotal", ct.SubtotalWithDiscount, 6.30, 0.02)
	t.Logf("  subtotal=%.2f discount=%.2f discounted=%.2f grand_total=%.2f",
		ct.SubtotalExcl, ct.DiscountAmount, ct.SubtotalWithDiscount, ct.GrandTotal)

	id := mustPlaceOrder(t, cartID)
	t.Logf("  order placed: %s", id)
	return id
}

// S05: Coupon FIXED5 — by_fixed $5 per unit
func runS05(t *testing.T) string {
	cartID := mustCreateCart(t)
	mustAddItems(t, cartID, []cartItem{
		{SKU: "24-MB03", Quantity: 2},
		{SKU: "24-WB01", Quantity: 1},
	})
	maybeApplyCoupon(t, cartID, "FIXED5")
	mustSetAddresses(t, cartID)
	mustSetShipping(t, cartID)
	mustSetPaymentAndEmail(t, cartID)

	ct := getCartTotals(t, cartID)
	approxEqual(t, "S05 subtotal", ct.SubtotalExcl, 108.00, 0.02)
	// $5 × 2qty + $5 × 1qty = $15
	approxEqual(t, "S05 discount", ct.DiscountAmount, 15.00, 0.02)
	approxEqual(t, "S05 discounted subtotal", ct.SubtotalWithDiscount, 93.00, 0.02)
	t.Logf("  subtotal=%.2f discount=%.2f discounted=%.2f grand_total=%.2f",
		ct.SubtotalExcl, ct.DiscountAmount, ct.SubtotalWithDiscount, ct.GrandTotal)

	id := mustPlaceOrder(t, cartID)
	t.Logf("  order placed: %s", id)
	return id
}

// S06: Coupon SAVE20 — cart_fixed $20 off cart total
func runS06(t *testing.T) string {
	cartID := mustCreateCart(t)
	mustAddItems(t, cartID, []cartItem{
		{SKU: "24-MB05", Quantity: 1},
		{SKU: "24-MB06", Quantity: 1},
	})
	maybeApplyCoupon(t, cartID, "SAVE20")
	mustSetAddresses(t, cartID)
	mustSetShipping(t, cartID)
	mustSetPaymentAndEmail(t, cartID)

	ct := getCartTotals(t, cartID)
	approxEqual(t, "S06 subtotal", ct.SubtotalExcl, 90.00, 0.02)
	// cart_fixed distributes $20: $10 per item (equal proportions)
	approxEqual(t, "S06 discount", ct.DiscountAmount, 20.00, 0.02)
	approxEqual(t, "S06 discounted subtotal", ct.SubtotalWithDiscount, 70.00, 0.02)
	t.Logf("  subtotal=%.2f discount=%.2f discounted=%.2f grand_total=%.2f",
		ct.SubtotalExcl, ct.DiscountAmount, ct.SubtotalWithDiscount, ct.GrandTotal)

	id := mustPlaceOrder(t, cartID)
	t.Logf("  order placed: %s", id)
	return id
}

// S07: Mixed — configurable hoodie + water bottle + coupon H20
// H20 applies to 24-UG06 only; MH01 is not discounted
func runS07(t *testing.T) string {
	cartID := mustCreateCart(t)
	mustAddItems(t, cartID, []cartItem{
		{
			SKU:      "MH01",
			Quantity: 1,
			SelectedOptions: []string{
				"Y29uZmlndXJhYmxlLzkzLzQ5",    // color: Black
				"Y29uZmlndXJhYmxlLzE0Mi8xNjY=", // size: XS
			},
		},
		{SKU: "24-UG06", Quantity: 2},
	})
	maybeApplyCoupon(t, cartID, "H20")
	mustSetAddresses(t, cartID)
	mustSetShipping(t, cartID)
	mustSetPaymentAndEmail(t, cartID)

	ct := getCartTotals(t, cartID)
	// MH01=$52 + 24-UG06×2=$14 → total $66
	approxEqual(t, "S07 subtotal", ct.SubtotalExcl, 66.00, 0.02)
	// H20 discounts only 24-UG06: 70% × $14 = $9.80
	approxEqual(t, "S07 discount", ct.DiscountAmount, 9.80, 0.02)
	approxEqual(t, "S07 discounted subtotal", ct.SubtotalWithDiscount, 56.20, 0.02)
	t.Logf("  subtotal=%.2f discount=%.2f discounted=%.2f grand_total=%.2f",
		ct.SubtotalExcl, ct.DiscountAmount, ct.SubtotalWithDiscount, ct.GrandTotal)

	id := mustPlaceOrder(t, cartID)
	t.Logf("  order placed: %s", id)
	return id
}

// ─── Main scenario runner ─────────────────────────────────────────────────────

// TestScenarios runs all 7 order placement scenarios sequentially and writes
// the resulting increment IDs to the admin test fixture file.
func TestScenarios(t *testing.T) {
	// Ensure test coupons FIXED5 and SAVE20 exist in the DB
	ensureTestCoupons(t)

	type scenario struct {
		id  string
		run func(*testing.T) string
	}
	scenarios := []scenario{
		{"S01_single_simple", runS01},
		{"S02_multi_item", runS02},
		{"S03_configurable", runS03},
		{"S04_coupon_h20_percent", runS04},
		{"S05_coupon_fixed5_per_item", runS05},
		{"S06_coupon_save20_cart_fixed", runS06},
		{"S07_mixed_configurable_coupon", runS07},
	}

	results := make(map[string]string, len(scenarios))
	var allOrders []string

	for _, s := range scenarios {
		s := s
		var incrementID string
		t.Run(s.id, func(t *testing.T) {
			incrementID = s.run(t)
		})
		results[s.id] = incrementID
		if incrementID != "" {
			allOrders = append(allOrders, incrementID)
		}
	}

	if t.Failed() {
		t.Log("One or more scenarios failed; fixture file will contain partial results.")
	}

	writeScenarioFixture(t, results, allOrders)
}

// writeScenarioFixture writes the placed order IDs to the admin test fixture file.
func writeScenarioFixture(t *testing.T, results map[string]string, allOrders []string) {
	t.Helper()

	// Resolve path: this file is in magento2-cart-graphql-go/tests/
	// fixture is at magento2-admin-tests/fixtures/orders.json
	_, thisFile, _, _ := runtime.Caller(0)
	fixtureDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "magento2-admin-tests", "fixtures")
	fixturePath := filepath.Join(fixtureDir, "orders.json")

	// Ensure the directory exists
	if err := os.MkdirAll(fixtureDir, 0755); err != nil {
		t.Logf("WARNING: could not create fixture dir %s: %v", fixtureDir, err)
		return
	}

	fixture := scenarioFixture{
		Comment:                 fmt.Sprintf("Written by TestScenarios on %s", time.Now().Format(time.RFC3339)),
		AllOrders:               allOrders,
		OrderForInvoiceShipment: results["S01_single_simple"],
		OrderForCreditMemo:      results["S02_multi_item"],
		Scenarios:               results,
	}

	data, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Logf("WARNING: could not marshal fixture: %v", err)
		return
	}

	if err := os.WriteFile(fixturePath, data, 0644); err != nil {
		t.Logf("WARNING: could not write fixture %s: %v", fixturePath, err)
		return
	}

	t.Logf("Fixture written to %s", fixturePath)
	t.Logf("  orderForInvoiceShipment: %s", fixture.OrderForInvoiceShipment)
	t.Logf("  orderForCreditMemo:      %s", fixture.OrderForCreditMemo)
	t.Logf("  allOrders: %v", allOrders)
}
