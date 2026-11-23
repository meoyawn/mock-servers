package stripemock

import "time"

const (
	subscriptionActive        = "active"
	subscriptionCanceled      = "canceled"
	subscriptionUpdateConfirm = "subscription_update_confirm"
	completionRedirect        = "redirect"
)

type object = map[string]any

type resourceList[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	HasMore bool   `json:"has_more"`
	URL     string `json:"url"`
}

func list[T any](data []T, path string) resourceList[T] {
	if data == nil {
		data = []T{}
	}
	return resourceList[T]{Object: "list", Data: data, URL: path}
}

type price struct {
	ID        string    `json:"id"`
	Object    string    `json:"object"`
	Product   string    `json:"product"`
	Currency  string    `json:"currency"`
	Amount    int64     `json:"unit_amount"`
	Active    bool      `json:"active"`
	Type      string    `json:"type"`
	Recurring recurring `json:"recurring"`
}

type recurring struct {
	Interval string `json:"interval"`
	Count    int    `json:"interval_count"`
}

type invoiceSettings struct {
	PaymentMethod *string `json:"default_payment_method"`
}

type customer struct {
	ID              string                       `json:"id"`
	Object          string                       `json:"object"`
	Email           string                       `json:"email"`
	Metadata        object                       `json:"metadata"`
	Balance         int64                        `json:"balance"`
	Currency        *string                      `json:"currency"`
	Livemode        bool                         `json:"livemode"`
	DefaultSource   any                          `json:"default_source"`
	InvoiceSettings invoiceSettings              `json:"invoice_settings"`
	Subscriptions   *resourceList[*subscription] `json:"subscriptions,omitempty"`
}

type subscription struct {
	ID                string                          `json:"id"`
	Object            string                          `json:"object"`
	Customer          string                          `json:"customer"`
	Status            string                          `json:"status"`
	Metadata          object                          `json:"metadata"`
	Livemode          bool                            `json:"livemode"`
	PaymentMethod     any                             `json:"default_payment_method"`
	DefaultSource     any                             `json:"default_source"`
	CancelAtPeriodEnd bool                            `json:"cancel_at_period_end"`
	Items             resourceList[*subscriptionItem] `json:"items"`
}

type subscriptionItem struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Price       *price `json:"price"`
	Quantity    int    `json:"quantity"`
	PeriodStart int64  `json:"current_period_start"`
	PeriodEnd   int64  `json:"current_period_end"`
}

func periodEnd(p *price) int64 {
	if p.Recurring.Interval == "year" {
		return time.Now().AddDate(1, 0, 0).Unix()
	}
	return time.Now().AddDate(0, 1, 0).Unix()
}

type checkoutItem struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Price    *price `json:"price"`
	Quantity int    `json:"quantity"`
}

type checkout struct {
	ID                      string
	CustomerID              string
	SubscriptionID          string
	PaymentMethodCollection string
	Metadata                object
	SubscriptionMetadata    object
	ClientReferenceID       string
	SuccessURL              string
	CancelURL               string
	URL                     string
	Item                    checkoutItem
	Status                  string
	PaymentStatus           string
	Completion              *completion
}

func (c *checkout) resource() object {
	var sub any
	if c.SubscriptionID != "" {
		sub = c.SubscriptionID
	}
	result := object{
		"id": c.ID, "object": "checkout.session", "customer": c.CustomerID,
		"mode": "subscription", "status": c.Status, "livemode": false,
		"payment_status": c.PaymentStatus, "payment_intent": nil, "subscription": sub,
		"payment_method_collection": c.PaymentMethodCollection, "metadata": c.Metadata,
		"subscription_metadata": c.SubscriptionMetadata, "client_reference_id": c.ClientReferenceID,
		"success_url": c.SuccessURL, "cancel_url": c.CancelURL, "url": c.URL,
		"line_items": list([]checkoutItem{c.Item}, "/v1/checkout/sessions/"+c.ID+"/line_items"),
	}
	if c.Completion != nil {
		result["completion_event"] = c.Completion.Event
		result["completion_url"] = c.Completion.URL
	}
	return result
}

type portal struct {
	ID         string
	CustomerID string
	ReturnURL  string
	URL        string
	Flow       *portalFlow
	Completion *completion
}

type portalFlow struct {
	Type            string             `json:"type"`
	AfterCompletion afterCompletion    `json:"after_completion"`
	Update          subscriptionUpdate `json:"subscription_update_confirm"`
}

type afterCompletion struct {
	Type     string   `json:"type"`
	Redirect redirect `json:"redirect"`
}

type redirect struct {
	ReturnURL string `json:"return_url"`
}

type subscriptionUpdate struct {
	Subscription string       `json:"subscription"`
	Items        []updateItem `json:"items"`
}

type updateItem struct {
	ID       string `json:"id"`
	Price    string `json:"price"`
	Quantity int    `json:"quantity"`
}

func (p *portal) resource() object {
	result := object{
		"id": p.ID, "object": "billing_portal.session", "customer": p.CustomerID,
		"livemode": false, "return_url": p.ReturnURL, "url": p.URL,
	}
	if p.Flow != nil {
		result["flow"] = p.Flow
	}
	if p.Completion != nil {
		result["completion_event"] = p.Completion.Event
		result["completion_url"] = p.Completion.URL
	}
	return result
}
