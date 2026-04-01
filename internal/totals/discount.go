package totals

import (
	"context"
	"math"

	"github.com/magendooro/magento2-cart-graphql-go/internal/repository"
)

// DiscountCollector applies cart price rule discounts to the cart totals.
// Sort order: 300 (runs after subtotal, before shipping and tax).
//
// Applies two kinds of rules:
//   - Coupon rules (coupon_type=2): the quote must carry the coupon code.
//   - Auto-apply rules (coupon_type=1): applied to every qualifying cart.
//
// For each rule the collector:
//  1. Evaluates conditions_serialized against the cart (subtotal, qty, address …)
//  2. Per eligible item, evaluates actions_serialized (sku, category, price …)
//  3. Computes and accumulates the item discount.
//  4. Stops further rules if stop_rules_processing is set.
type DiscountCollector struct {
	CouponRepo *repository.CouponRepository
}

func (c *DiscountCollector) Code() string { return "discount" }

func (c *DiscountCollector) Collect(ctx context.Context, cc *CollectorContext, total *Total) error {
	// Pre-load category IDs for every top-level item (used by both cart
	// conditions and action conditions so load once, reuse everywhere).
	categoryCache := make(map[int][]int)
	loadCategories := func(productID int) []int {
		if cats, ok := categoryCache[productID]; ok {
			return cats
		}
		cats := c.CouponRepo.GetItemCategoryIDs(ctx, productID)
		categoryCache[productID] = cats
		return cats
	}

	// Build cart eval context (includes per-item eval contexts for Product\Found).
	cartCtx := c.buildCartContext(cc, total, loadCategories)

	// Collect rules to apply, coupon rule first.
	var rules []*repository.SalesRule

	if cc.Quote.CouponCode != nil && *cc.Quote.CouponCode != "" {
		_, rule, err := c.CouponRepo.LookupCoupon(ctx, *cc.Quote.CouponCode, 1, cc.Quote.CustomerGroupID)
		if err == nil {
			rules = append(rules, rule)
		}
	}

	autoRules, _ := c.CouponRepo.GetAutoApplyRules(ctx, 1, cc.Quote.CustomerGroupID)
	rules = append(rules, autoRules...)

	if len(rules) == 0 {
		return nil
	}

	for _, rule := range rules {
		// Evaluate cart-level conditions (subtotal thresholds, country, etc.).
		condRoot := repository.ParseConditionTree(rule.ConditionsSerialized)
		if !repository.EvaluateCartConditions(condRoot, cartCtx) {
			continue
		}

		actRoot := repository.ParseConditionTree(rule.ActionsSerialized)

		if rule.SimpleAction == "buy_x_get_y" {
			c.applyBuyXGetY(rule, actRoot, cc.Items, loadCategories, total)
		} else {
			c.applyPerItemDiscount(rule, actRoot, cc.Items, loadCategories, total)
		}

		if rule.StopRulesProcessing == 1 {
			break
		}
	}

	return nil
}

// buildCartContext assembles a CartEvalContext from the collector context.
func (c *DiscountCollector) buildCartContext(
	cc *CollectorContext,
	total *Total,
	loadCategories func(int) []int,
) repository.CartEvalContext {
	ctx := repository.CartEvalContext{
		Subtotal: total.Subtotal,
	}

	for _, item := range cc.Items {
		if item.ParentItemID != nil {
			continue
		}
		ctx.TotalQty += item.Qty
		ctx.TotalWeight += item.Weight * item.Qty
		ctx.Items = append(ctx.Items, repository.ItemEvalContext{
			SKU:         item.SKU,
			ProductType: item.ProductType,
			Price:       item.Price,
			CategoryIDs: loadCategories(item.ProductID),
		})
	}

	if cc.Address != nil {
		ctx.CountryID = cc.Address.CountryID
		if cc.Address.RegionID != nil {
			ctx.RegionID = *cc.Address.RegionID
		}
		if cc.Address.Region != nil {
			ctx.Region = *cc.Address.Region
		}
		if cc.Address.Postcode != nil {
			ctx.Postcode = *cc.Address.Postcode
		}
		if cc.Address.ShippingMethod != nil {
			ctx.ShippingMethod = *cc.Address.ShippingMethod
		}
	}

	return ctx
}

// applyPerItemDiscount handles by_percent, by_fixed, cart_fixed, to_percent, to_fixed.
func (c *DiscountCollector) applyPerItemDiscount(
	rule *repository.SalesRule,
	actRoot repository.ConditionNode,
	items []*repository.CartItemData,
	loadCategories func(int) []int,
	total *Total,
) {
	for _, item := range items {
		if item.ParentItemID != nil {
			continue
		}
		if !repository.EvaluateItemMatchesActions(actRoot, repository.ItemEvalContext{
			SKU:         item.SKU,
			ProductType: item.ProductType,
			Price:       item.Price,
			CategoryIDs: loadCategories(item.ProductID),
		}) {
			continue
		}

		var itemDiscount float64
		switch rule.SimpleAction {
		case "by_percent":
			itemDiscount = item.RowTotal * rule.DiscountAmount / 100.0
		case "by_fixed":
			itemDiscount = rule.DiscountAmount * item.Qty
		case "cart_fixed":
			// Distribute proportionally across eligible items.
			if total.Subtotal > 0 {
				itemDiscount = rule.DiscountAmount * (item.RowTotal / total.Subtotal)
			}
		case "to_percent":
			// Discount is what must be removed so item total reaches N% of original.
			itemDiscount = item.RowTotal * (1 - rule.DiscountAmount/100.0)
		case "to_fixed":
			// Discount is what must be removed so per-unit price reaches DiscountAmount.
			itemDiscount = math.Max(0, item.RowTotal-rule.DiscountAmount*item.Qty)
		}

		itemDiscount = math.Round(itemDiscount*100) / 100
		if itemDiscount > item.RowTotal {
			itemDiscount = item.RowTotal
		}
		total.DiscountAmount += itemDiscount
	}
}

// applyBuyXGetY handles the buy_x_get_y action.
// discount_step = X (units to buy), discount_amount = Y (units given free).
// For every complete set of (X+Y) units, Y units are free (100% off).
// Partial trailing units: anything beyond the X threshold in the last partial set is also free.
func (c *DiscountCollector) applyBuyXGetY(
	rule *repository.SalesRule,
	actRoot repository.ConditionNode,
	items []*repository.CartItemData,
	loadCategories func(int) []int,
	total *Total,
) {
	step := float64(rule.DiscountStep) // X
	free := rule.DiscountAmount        // Y
	if step <= 0 || free <= 0 {
		return
	}
	setSize := step + free

	for _, item := range items {
		if item.ParentItemID != nil {
			continue
		}
		if !repository.EvaluateItemMatchesActions(actRoot, repository.ItemEvalContext{
			SKU:         item.SKU,
			ProductType: item.ProductType,
			Price:       item.Price,
			CategoryIDs: loadCategories(item.ProductID),
		}) {
			continue
		}

		// Complete sets
		sets := math.Floor(item.Qty / setSize)
		// Remainder beyond complete sets: anything > step is also free
		remainder := item.Qty - sets*setSize
		freeQty := sets*free + math.Max(0, remainder-step)

		itemDiscount := math.Round(item.Price*freeQty*100) / 100
		if itemDiscount > item.RowTotal {
			itemDiscount = item.RowTotal
		}
		total.DiscountAmount += itemDiscount
	}
}
