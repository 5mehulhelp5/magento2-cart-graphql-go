package repository

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// ConditionNode mirrors Magento's serialized condition tree stored in
// conditions_serialized and actions_serialized on the salesrule table.
type ConditionNode struct {
	Type       string          `json:"type"`
	Attribute  *string         `json:"attribute"`
	Operator   string          `json:"operator"`
	Value      string          `json:"value"`
	Aggregator string          `json:"aggregator"` // "all" | "any"
	Conditions []ConditionNode `json:"conditions"`
}

// ParseConditionTree unmarshals a Magento conditions JSON string.
// Returns an empty match-all Combine on any error.
func ParseConditionTree(raw string) ConditionNode {
	if raw == "" {
		return matchAllCombine()
	}
	var node ConditionNode
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		return matchAllCombine()
	}
	return node
}

func matchAllCombine() ConditionNode {
	return ConditionNode{
		Type:       "Magento\\SalesRule\\Model\\Rule\\Condition\\Combine",
		Value:      "1",
		Aggregator: "all",
	}
}

// CartEvalContext holds cart-level state for evaluating conditions_serialized.
type CartEvalContext struct {
	Subtotal       float64
	TotalQty       float64
	TotalWeight    float64
	CountryID      string
	RegionID       int
	Region         string
	Postcode       string
	ShippingMethod string
	PaymentMethod  string // may be empty; unknown → don't block
	Items          []ItemEvalContext
}

// ItemEvalContext holds per-item attributes for evaluating actions_serialized
// and Product\Found cart conditions.
type ItemEvalContext struct {
	SKU         string
	ProductType string
	Price       float64
	CategoryIDs []int
}

// EvaluateCartConditions returns true when the cart satisfies the rule's
// conditions_serialized tree. An empty tree always returns true.
func EvaluateCartConditions(root ConditionNode, ctx CartEvalContext) bool {
	return evalCartNode(root, ctx)
}

// EvaluateItemMatchesActions returns true when the item should receive the
// rule's discount, according to the rule's actions_serialized tree.
// An empty tree means "apply to all products".
func EvaluateItemMatchesActions(root ConditionNode, item ItemEvalContext) bool {
	return evalItemNode(root, item)
}

// ─── Class helpers ───────────────────────────────────────────────────────────

// condClass returns the short class suffix from a fully-qualified PHP name,
// normalised to use backslashes.
// e.g. "Magento\SalesRule\Model\Rule\Condition\Product\Found" → "Product\Found"
func condClass(typ string) string {
	const prefix = `Magento\SalesRule\Model\Rule\Condition\`
	s := strings.TrimPrefix(typ, prefix)
	return strings.ReplaceAll(s, "/", `\`)
}

// ─── Cart-level evaluation ───────────────────────────────────────────────────

func evalCartNode(node ConditionNode, ctx CartEvalContext) bool {
	cls := condClass(node.Type)
	switch cls {
	case "Combine":
		return evalCartCombine(node, ctx)
	case "Address":
		return evalAddressLeaf(node, ctx)
	case `Product\Found`:
		return evalProductFound(node, ctx)
	default:
		return true // unknown type → do not block
	}
}

func evalCartCombine(node ConditionNode, ctx CartEvalContext) bool {
	wantTrue := node.Value != "0"
	allMatch := node.Aggregator == "" || node.Aggregator == "all"

	if len(node.Conditions) == 0 {
		return wantTrue
	}
	for _, child := range node.Conditions {
		childPasses := evalCartNode(child, ctx)
		if allMatch && !childPasses {
			return !wantTrue
		}
		if !allMatch && childPasses {
			return wantTrue
		}
	}
	if allMatch {
		return wantTrue
	}
	return !wantTrue
}

func evalAddressLeaf(node ConditionNode, ctx CartEvalContext) bool {
	if node.Attribute == nil {
		return true
	}
	attr := *node.Attribute
	op, val := node.Operator, node.Value

	switch attr {
	case "base_subtotal", "base_subtotal_with_discount", "base_subtotal_total_incl_tax":
		return evalNumericOp(ctx.Subtotal, op, val)
	case "total_qty":
		return evalNumericOp(ctx.TotalQty, op, val)
	case "weight":
		return evalNumericOp(ctx.TotalWeight, op, val)
	case "country_id":
		return evalStringOp(ctx.CountryID, op, val)
	case "region":
		return evalStringOp(ctx.Region, op, val)
	case "region_id":
		return evalNumericOp(float64(ctx.RegionID), op, val)
	case "postcode":
		return evalStringOp(ctx.Postcode, op, val)
	case "shipping_method":
		return evalStringOp(ctx.ShippingMethod, op, val)
	case "payment_method":
		if ctx.PaymentMethod == "" {
			return true
		}
		return evalStringOp(ctx.PaymentMethod, op, val)
	}
	return true // unknown attribute → do not block
}

// evalProductFound: "If a product IS/IS NOT found in the cart matching ALL/ANY
// of these conditions." value="1"→found, value="0"→not found.
func evalProductFound(node ConditionNode, ctx CartEvalContext) bool {
	wantFound := node.Value != "0"
	allMatch := node.Aggregator == "" || node.Aggregator == "all"

	for _, item := range ctx.Items {
		if evalItemConditionSet(node.Conditions, allMatch, item) {
			return wantFound
		}
	}
	return !wantFound
}

// evalItemConditionSet tests a flat list of leaf/combine conditions against
// one item, using the given ALL/ANY aggregator.
func evalItemConditionSet(conditions []ConditionNode, allMatch bool, item ItemEvalContext) bool {
	if len(conditions) == 0 {
		return true
	}
	for _, cond := range conditions {
		passes := evalItemNode(cond, item)
		if allMatch && !passes {
			return false
		}
		if !allMatch && passes {
			return true
		}
	}
	return allMatch
}

// ─── Item-level evaluation ───────────────────────────────────────────────────

func evalItemNode(node ConditionNode, item ItemEvalContext) bool {
	cls := condClass(node.Type)
	if strings.Contains(cls, "Combine") || strings.Contains(cls, "Found") {
		return evalItemCombine(node, item)
	}
	return evalProductLeaf(node, item)
}

func evalItemCombine(node ConditionNode, item ItemEvalContext) bool {
	wantTrue := node.Value != "0"
	allMatch := node.Aggregator == "" || node.Aggregator == "all"

	if len(node.Conditions) == 0 {
		return wantTrue
	}
	for _, child := range node.Conditions {
		childPasses := evalItemNode(child, item)
		if allMatch && !childPasses {
			return !wantTrue
		}
		if !allMatch && childPasses {
			return wantTrue
		}
	}
	if allMatch {
		return wantTrue
	}
	return !wantTrue
}

func evalProductLeaf(node ConditionNode, item ItemEvalContext) bool {
	if node.Attribute == nil {
		return true
	}
	attr := *node.Attribute
	op, val := node.Operator, node.Value

	// Strip parent:: / children:: scope prefixes (treat as same attribute)
	if i := strings.LastIndex(attr, "::"); i >= 0 {
		attr = attr[i+2:]
	}

	switch attr {
	case "sku":
		return evalStringOp(item.SKU, op, val)
	case "type_id":
		return evalStringOp(item.ProductType, op, val)
	case "price":
		return evalNumericOp(item.Price, op, val)
	case "category_ids":
		return evalCategoryOp(item.CategoryIDs, op, val)
	default:
		return true // unknown product attribute → do not block
	}
}

// ─── Operator evaluators ─────────────────────────────────────────────────────

func evalNumericOp(cartVal float64, op, ruleVal string) bool {
	rv, err := strconv.ParseFloat(strings.TrimSpace(ruleVal), 64)
	if err != nil {
		return true
	}
	const eps = 0.0001
	switch op {
	case "==":
		return math.Abs(cartVal-rv) < eps
	case "!=":
		return math.Abs(cartVal-rv) >= eps
	case ">=":
		return cartVal >= rv
	case ">":
		return cartVal > rv
	case "<=":
		return cartVal <= rv
	case "<":
		return cartVal < rv
	case "()", "{}":
		for _, v := range parseFloatList(ruleVal) {
			if math.Abs(cartVal-v) < eps {
				return true
			}
		}
		return false
	case "!()", "!{}":
		for _, v := range parseFloatList(ruleVal) {
			if math.Abs(cartVal-v) < eps {
				return false
			}
		}
		return true
	}
	return true
}

func evalStringOp(cartVal, op, ruleVal string) bool {
	low := strings.ToLower(cartVal)
	switch op {
	case "==", "<=>":
		return low == strings.ToLower(ruleVal)
	case "!=":
		return low != strings.ToLower(ruleVal)
	case ">=":
		return cartVal >= ruleVal
	case ">":
		return cartVal > ruleVal
	case "<=":
		return cartVal <= ruleVal
	case "<":
		return cartVal < ruleVal
	case "{}":
		return strings.Contains(low, strings.ToLower(ruleVal))
	case "!{}":
		return !strings.Contains(low, strings.ToLower(ruleVal))
	case "()":
		for _, v := range strings.Split(ruleVal, ",") {
			if strings.TrimSpace(strings.ToLower(v)) == low {
				return true
			}
		}
		return false
	case "!()":
		for _, v := range strings.Split(ruleVal, ",") {
			if strings.TrimSpace(strings.ToLower(v)) == low {
				return false
			}
		}
		return true
	}
	return true
}

// evalCategoryOp handles multi-value category_ids comparisons.
// ruleVal is a comma-separated list of category IDs.
func evalCategoryOp(catIDs []int, op, ruleVal string) bool {
	ruleCats := parseIntList(ruleVal)
	itemHas := func(rCat int) bool {
		for _, iCat := range catIDs {
			if iCat == rCat {
				return true
			}
		}
		return false
	}
	switch op {
	case "==", "()", "{}":
		for _, rc := range ruleCats {
			if itemHas(rc) {
				return true
			}
		}
		return false
	case "!=", "!()", "!{}":
		for _, rc := range ruleCats {
			if itemHas(rc) {
				return false
			}
		}
		return true
	}
	return true
}

// ─── Parse helpers ────────────────────────────────────────────────────────────

func parseIntList(s string) []int {
	var result []int
	for _, part := range strings.Split(s, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			result = append(result, v)
		}
	}
	return result
}

func parseFloatList(s string) []float64 {
	var result []float64
	for _, part := range strings.Split(s, ",") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(part), 64); err == nil {
			result = append(result, v)
		}
	}
	return result
}
