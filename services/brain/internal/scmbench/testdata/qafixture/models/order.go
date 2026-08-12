package models

// Order is a purchase record belonging to a user. Orders move through a small
// state machine from placed to fulfilled or cancelled.
type Order struct {
	ID     int
	UserID int
	Total  int64
	Status OrderStatus
	Items  []OrderItem
}

// OrderStatus names the lifecycle states of an order.
type OrderStatus string

// Order lifecycle states.
const (
	OrderPlaced    OrderStatus = "placed"
	OrderPaid      OrderStatus = "paid"
	OrderFulfilled OrderStatus = "fulfilled"
	OrderCancelled OrderStatus = "cancelled"
)

// OrderItem is one line within an order.
type OrderItem struct {
	SKU       string
	Quantity  int
	UnitCents int64
}

// TotalCents recomputes the order total from its items.
func (o *Order) TotalCents() int64 {
	var total int64
	for _, it := range o.Items {
		total += int64(it.Quantity) * it.UnitCents
	}
	o.Total = total
	return total
}

// CanCancel reports whether the order may still be cancelled.
func (o Order) CanCancel() bool {
	return o.Status == OrderPlaced || o.Status == OrderPaid
}
