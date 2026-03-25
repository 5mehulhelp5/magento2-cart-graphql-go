package repository

import (
	"database/sql"
)

// OrderRepository holds the DB connection for order placement.
// The actual conversion and SQL logic lives in internal/order.
type OrderRepository struct {
	db *sql.DB
}

func NewOrderRepository(db *sql.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

// DB returns the underlying *sql.DB for use by order.Place.
func (r *OrderRepository) DB() *sql.DB { return r.db }
