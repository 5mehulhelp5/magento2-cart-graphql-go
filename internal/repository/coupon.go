package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SalesRule holds a cart price rule.
type SalesRule struct {
	RuleID               int
	Name                 string
	IsActive             int
	SimpleAction         string  // by_percent, by_fixed, cart_fixed, to_percent, to_fixed, buy_x_get_y
	DiscountAmount       float64
	DiscountQty          *float64
	DiscountStep         int     // X in "buy X get Y free"
	CouponType           int     // 1=no coupon (auto-apply), 2=specific coupon, 3=auto-generated
	FromDate             *time.Time
	ToDate               *time.Time
	StopRulesProcessing  int
	SortOrder            int
	UsesPerCoupon        int
	UsesPerCustomer      int
	ApplyToShipping      int
	Description          *string
	ConditionsSerialized string // JSON: conditions_serialized
	ActionsSerialized    string // JSON: actions_serialized
}

// SalesRuleCoupon holds a coupon code linked to a rule.
type SalesRuleCoupon struct {
	CouponID   int
	RuleID     int
	Code       string
	UsageLimit *int
	TimesUsed  int
}

type CouponRepository struct {
	db *sql.DB
}

func NewCouponRepository(db *sql.DB) *CouponRepository {
	return &CouponRepository{db: db}
}

// ruleColumns is the SELECT column list for loading a full SalesRule row.
const ruleColumns = `
	rule_id, name, is_active, COALESCE(simple_action, ''), COALESCE(discount_amount, 0),
	discount_qty, coupon_type, from_date, to_date, stop_rules_processing,
	sort_order, uses_per_coupon, uses_per_customer, apply_to_shipping, description,
	COALESCE(discount_step, 0),
	COALESCE(conditions_serialized, ''),
	COALESCE(actions_serialized, '')`

func scanRule(s interface {
	Scan(...any) error
}, rule *SalesRule) error {
	return s.Scan(
		&rule.RuleID, &rule.Name, &rule.IsActive, &rule.SimpleAction, &rule.DiscountAmount,
		&rule.DiscountQty, &rule.CouponType, &rule.FromDate, &rule.ToDate, &rule.StopRulesProcessing,
		&rule.SortOrder, &rule.UsesPerCoupon, &rule.UsesPerCustomer, &rule.ApplyToShipping, &rule.Description,
		&rule.DiscountStep, &rule.ConditionsSerialized, &rule.ActionsSerialized,
	)
}

// LookupCoupon validates a coupon code and returns the coupon + rule.
// Returns an error if the coupon doesn't exist, is over limit, or the rule
// is inactive, expired, or unavailable for the given website/customer group.
func (r *CouponRepository) LookupCoupon(ctx context.Context, code string, websiteID, customerGroupID int) (*SalesRuleCoupon, *SalesRule, error) {
	var coupon SalesRuleCoupon
	err := r.db.QueryRowContext(ctx,
		"SELECT coupon_id, rule_id, code, usage_limit, times_used FROM salesrule_coupon WHERE code = ?",
		code,
	).Scan(&coupon.CouponID, &coupon.RuleID, &coupon.Code, &coupon.UsageLimit, &coupon.TimesUsed)
	if err != nil {
		return nil, nil, fmt.Errorf("coupon not found")
	}

	if coupon.UsageLimit != nil && *coupon.UsageLimit > 0 && coupon.TimesUsed >= *coupon.UsageLimit {
		return nil, nil, fmt.Errorf("coupon usage limit exceeded")
	}

	var rule SalesRule
	err = scanRule(r.db.QueryRowContext(ctx,
		"SELECT"+ruleColumns+" FROM salesrule WHERE rule_id = ?",
		coupon.RuleID,
	), &rule)
	if err != nil {
		return nil, nil, fmt.Errorf("rule not found")
	}

	if rule.IsActive != 1 {
		return nil, nil, fmt.Errorf("rule not active")
	}
	now := time.Now()
	if rule.FromDate != nil && now.Before(*rule.FromDate) {
		return nil, nil, fmt.Errorf("rule not yet active")
	}
	if rule.ToDate != nil && now.After(*rule.ToDate) {
		return nil, nil, fmt.Errorf("rule expired")
	}

	var websiteCount int
	r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM salesrule_website WHERE rule_id = ? AND website_id = ?",
		rule.RuleID, websiteID,
	).Scan(&websiteCount)
	if websiteCount == 0 {
		return nil, nil, fmt.Errorf("rule not available for website")
	}

	var groupCount int
	r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM salesrule_customer_group WHERE rule_id = ? AND customer_group_id = ?",
		rule.RuleID, customerGroupID,
	).Scan(&groupCount)
	if groupCount == 0 {
		return nil, nil, fmt.Errorf("rule not available for customer group")
	}

	return &coupon, &rule, nil
}

// GetAutoApplyRules returns all active auto-apply rules (coupon_type=1) for
// the given website and customer group, ordered by sort_order.
func (r *CouponRepository) GetAutoApplyRules(ctx context.Context, websiteID, customerGroupID int) ([]*SalesRule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT`+ruleColumns+`
		FROM salesrule r
		JOIN salesrule_website rw ON rw.rule_id = r.rule_id AND rw.website_id = ?
		JOIN salesrule_customer_group rg ON rg.rule_id = r.rule_id AND rg.customer_group_id = ?
		WHERE r.is_active = 1 AND r.coupon_type = 1
		  AND (r.from_date IS NULL OR r.from_date <= CURDATE())
		  AND (r.to_date IS NULL OR r.to_date >= CURDATE())
		ORDER BY r.sort_order ASC, r.rule_id ASC`,
		websiteID, customerGroupID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []*SalesRule
	for rows.Next() {
		var rule SalesRule
		if err := scanRule(rows, &rule); err != nil {
			continue
		}
		rules = append(rules, &rule)
	}
	return rules, rows.Err()
}

// GetItemCategoryIDs returns the category IDs assigned to a product.
func (r *CouponRepository) GetItemCategoryIDs(ctx context.Context, productID int) []int {
	rows, err := r.db.QueryContext(ctx,
		"SELECT category_id FROM catalog_category_product WHERE product_id = ?",
		productID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// SetCouponOnQuote stores the coupon code and applied rule IDs on the quote.
func (r *CouponRepository) SetCouponOnQuote(ctx context.Context, quoteID int, couponCode string, appliedRuleIDs string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote SET coupon_code = ?, applied_rule_ids = ?, updated_at = NOW() WHERE entity_id = ?",
		couponCode, appliedRuleIDs, quoteID,
	)
	return err
}

// ClearCouponOnQuote removes the coupon from the quote.
func (r *CouponRepository) ClearCouponOnQuote(ctx context.Context, quoteID int) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote SET coupon_code = NULL, applied_rule_ids = NULL, updated_at = NOW() WHERE entity_id = ?",
		quoteID,
	)
	return err
}

// UpdateItemDiscount sets the discount on a quote item.
func (r *CouponRepository) UpdateItemDiscount(ctx context.Context, itemID int, discountAmount, discountPercent float64, appliedRuleIDs string) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote_item SET discount_amount = ?, discount_percent = ?, applied_rule_ids = ?, updated_at = NOW() WHERE item_id = ?",
		discountAmount, discountPercent, appliedRuleIDs, itemID,
	)
	return err
}

// ClearItemDiscounts resets discount on all items for a quote.
func (r *CouponRepository) ClearItemDiscounts(ctx context.Context, quoteID int) error {
	_, err := r.db.ExecContext(ctx,
		"UPDATE quote_item SET discount_amount = 0, discount_percent = 0, applied_rule_ids = NULL, updated_at = NOW() WHERE quote_id = ?",
		quoteID,
	)
	return err
}
