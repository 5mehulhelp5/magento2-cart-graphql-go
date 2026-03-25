package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/magendooro/magento2-go-common/config"
)

// PaymentMethod holds an available payment method.
type PaymentMethod struct {
	Code  string
	Title string
}

type PaymentRepository struct {
	db *sql.DB
	cp *config.ConfigProvider
}

func NewPaymentRepository(db *sql.DB, cp *config.ConfigProvider) *PaymentRepository {
	return &PaymentRepository{db: db, cp: cp}
}

// GetAvailableMethods returns active payment methods for the given store.
func (r *PaymentRepository) GetAvailableMethods(ctx context.Context, storeID int, grandTotal float64) []*PaymentMethod {
	var methods []*PaymentMethod

	// Check/Money Order
	if r.cp.GetBool("payment/checkmo/active", storeID) || r.cp.Get("payment/checkmo/active", storeID) == "" {
		// checkmo is active by default even if not in core_config_data
		title := r.cp.Get("payment/checkmo/title", storeID)
		if title == "" {
			title = "Check / Money order"
		}
		methods = append(methods, &PaymentMethod{Code: "checkmo", Title: title})
	}

	// Free payment (for zero-total carts)
	if grandTotal == 0 {
		methods = append(methods, &PaymentMethod{Code: "free", Title: "No Payment Information Required"})
	}

	// Bank Transfer
	if r.cp.GetBool("payment/banktransfer/active", storeID) {
		title := r.cp.Get("payment/banktransfer/title", storeID)
		if title == "" {
			title = "Bank Transfer Payment"
		}
		methods = append(methods, &PaymentMethod{Code: "banktransfer", Title: title})
	}

	// Cash On Delivery
	if r.cp.GetBool("payment/cashondelivery/active", storeID) {
		title := r.cp.Get("payment/cashondelivery/title", storeID)
		if title == "" {
			title = "Cash On Delivery"
		}
		methods = append(methods, &PaymentMethod{Code: "cashondelivery", Title: title})
	}

	// Purchase Order
	if r.cp.GetBool("payment/purchaseorder/active", storeID) {
		title := r.cp.Get("payment/purchaseorder/title", storeID)
		if title == "" {
			title = "Purchase Order"
		}
		methods = append(methods, &PaymentMethod{Code: "purchaseorder", Title: title})
	}

	return methods
}

// SetPaymentMethod stores the selected payment method on the cart.
func (r *PaymentRepository) SetPaymentMethod(ctx context.Context, quoteID int, methodCode string) error {
	// Check if payment already exists
	var existingID int
	err := r.db.QueryRowContext(ctx, "SELECT payment_id FROM quote_payment WHERE quote_id = ?", quoteID).Scan(&existingID)
	if err == nil {
		_, err = r.db.ExecContext(ctx, "UPDATE quote_payment SET method = ? WHERE payment_id = ?", methodCode, existingID)
		return err
	}

	_, err = r.db.ExecContext(ctx,
		"INSERT INTO quote_payment (quote_id, method, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		quoteID, methodCode,
	)
	return err
}

// GetSelectedMethod returns the selected payment method for a cart.
func (r *PaymentRepository) GetSelectedMethod(ctx context.Context, quoteID int) (*PaymentMethod, error) {
	var code string
	err := r.db.QueryRowContext(ctx, "SELECT method FROM quote_payment WHERE quote_id = ?", quoteID).Scan(&code)
	if err != nil {
		return nil, fmt.Errorf("no payment method set: %w", err)
	}
	return &PaymentMethod{Code: code, Title: code}, nil
}
