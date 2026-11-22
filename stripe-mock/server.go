// Package stripemock implements the Stripe billing boundary used by integrated E2E tests.
package stripemock

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	customerResourceName     = "customer"
	subscriptionResourceName = "subscription"
	objectField              = "object"
	maxFormBytes             = 1 << 20
	maxWebhookResponseBytes  = 4096
)

type Price struct {
	ID        string
	ProductID string
	Currency  string
	Amount    int64
	Interval  string
}

type Config struct {
	Origin               string
	SecretKey            string
	WebhookURL           string
	WebhookSigningSecret string
	HTTPClient           *http.Client
	Prices               []Price
}

type receipt struct {
	params string
	body   []byte
}

type event struct {
	ID       string    `json:"id"`
	Object   string    `json:"object"`
	Type     string    `json:"type"`
	Created  int64     `json:"created"`
	Livemode bool      `json:"livemode"`
	Data     eventData `json:"data"`
}

type eventData struct {
	Object json.RawMessage `json:"object"`
}

type completion struct {
	Event *event
	URL   string
}

// requestError retains Stripe's protocol message capitalization on HTTP errors.
type requestError string

func (e requestError) Error() string { return string(e) }

type Server struct {
	mu            sync.Mutex
	config        Config
	next          uint64
	prices        map[string]*price
	customers     map[string]*customer
	subscriptions map[string]*subscription
	checkouts     map[string]*checkout
	portals       map[string]*portal
	events        []*event
	receipts      map[string]receipt
}

func New(config Config) (*Server, error) {
	for name, raw := range map[string]string{"origin": config.Origin, "webhook URL": config.WebhookURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("stripe mock %s %q must be an HTTP URL", name, raw)
		}
	}
	if config.SecretKey == "" || config.WebhookSigningSecret == "" || config.HTTPClient == nil {
		return nil, errors.New("stripe mock requires credentials and an HTTP client")
	}
	s := &Server{
		config: config, prices: map[string]*price{}, customers: map[string]*customer{},
		subscriptions: map[string]*subscription{}, checkouts: map[string]*checkout{},
		portals: map[string]*portal{}, receipts: map[string]receipt{},
	}
	for _, p := range config.Prices {
		interval := "month"
		if p.Interval == "annual" {
			interval = "year"
		}
		s.prices[p.ID] = &price{
			ID:        p.ID,
			Object:    "price",
			Product:   p.ProductID,
			Currency:  p.Currency,
			Amount:    p.Amount,
			Active:    true,
			Type:      "recurring",
			Recurring: recurring{Interval: interval, Count: 1},
		}
	}
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/checkout/") || strings.HasPrefix(r.URL.Path, "/portal/") {
		s.hosted(w, r)
		return
	}
	if !s.authorized(r) {
		invalid(w, http.StatusUnauthorized, "Invalid API key")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		invalid(w, http.StatusBadRequest, "Invalid form body")
		return
	}
	s.serveAPI(w, r)
}

func (s *Server) authorized(r *http.Request) bool {
	user, _, basic := r.BasicAuth()
	return r.Header.Get("Authorization") == "Bearer "+s.config.SecretKey ||
		(basic && user == s.config.SecretKey)
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Request-Id", s.id("req"))
	key := r.Header.Get("Idempotency-Key")
	fingerprint := r.Method + " " + r.URL.Path + "?" + r.Form.Encode()
	if key != "" && r.Method == http.MethodPost {
		if previous, ok := s.receipts[key]; ok {
			if previous.params != fingerprint {
				invalid(w, http.StatusConflict, "Idempotency key reused with different parameters")
				return
			}
			writeJSONBytes(w, previous.body)
			return
		}
	}
	value, err := s.api(r)
	if err != nil {
		invalid(w, http.StatusBadRequest, err.Error())
		return
	}
	if value == nil {
		invalid(w, http.StatusNotFound, "Unknown Stripe resource: "+r.Method+" "+r.URL.Path)
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		invalid(w, http.StatusInternalServerError, err.Error())
		return
	}
	if key != "" && r.Method == http.MethodPost {
		s.receipts[key] = receipt{fingerprint, data}
	}
	writeJSONBytes(w, data)
}

func (s *Server) id(prefix string) string {
	s.next++
	return fmt.Sprintf("%s_mock%016x", prefix, s.next)
}

func (s *Server) event(kind string, value any) (*event, error) {
	// Capture the resource now so later state changes cannot rewrite an earlier webhook.
	snapshot, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("snapshot Stripe event %q: %w", kind, err)
	}
	e := &event{
		ID:      s.id("evt"),
		Object:  "event",
		Type:    kind,
		Created: time.Now().Unix(),
		Data:    eventData{Object: snapshot},
	}
	s.events = append(s.events, e)
	return e, nil
}

func (s *Server) customer(id string, expand bool) *customer {
	c := s.customers[id]
	if c == nil {
		return nil
	}
	result := *c
	if expand {
		var subscriptions []*subscription
		for _, sub := range s.subscriptions {
			if sub.Customer == id && sub.Status != subscriptionCanceled {
				subscriptions = append(subscriptions, sub)
			}
		}
		slices.SortFunc(
			subscriptions,
			func(a, b *subscription) int { return cmp.Compare(a.ID, b.ID) },
		)
		items := list(subscriptions, "/v1/subscriptions")
		result.Subscriptions = &items
	}
	return &result
}

func (s *Server) api(r *http.Request) (any, error) {
	path := r.URL.Path
	switch {
	case path == "/v1/customers":
		return s.customerCollection(r)
	case strings.HasPrefix(path, "/v1/customers/"):
		return s.customerResource(r)
	case strings.HasPrefix(path, "/v1/subscriptions/"):
		return s.subscriptionResource(r)
	case strings.HasPrefix(path, "/v1/checkout/sessions/"):
		if r.Method == http.MethodGet {
			return s.checkoutResource(r)
		}
		return nil, nil
	}
	switch r.Method + " " + path {
	case "POST /v1/subscriptions":
		return s.createSubscription(
			r.Form.Get(customerResourceName),
			r.Form.Get("items[0][price]"),
			metadata(r.Form, "metadata"),
			r.Form.Get("payment_behavior"),
		)
	case "POST /v1/checkout/sessions":
		return s.createCheckout(r.Form)
	case "POST /v1/billing_portal/sessions":
		return s.createPortal(r.Form)
	case "GET /v1/events":
		return s.listEvents(r.Form), nil
	default:
		return nil, nil
	}
}

func (s *Server) customerCollection(r *http.Request) (any, error) {
	switch r.Method {
	case http.MethodPost:
		c := &customer{
			ID:       s.id("cus"),
			Object:   customerResourceName,
			Email:    r.Form.Get("email"),
			Metadata: metadata(r.Form, "metadata"),
		}
		if pm := r.Form.Get("invoice_settings[default_payment_method]"); pm != "" {
			c.InvoiceSettings.PaymentMethod = &pm
		}
		s.customers[c.ID] = c
		return c, nil
	case http.MethodGet:
		var data []*customer
		for id, c := range s.customers {
			if r.Form.Get("email") == "" || c.Email == r.Form.Get("email") {
				data = append(data, s.customer(id, false))
			}
		}
		slices.SortFunc(data, func(a, b *customer) int { return cmp.Compare(a.ID, b.ID) })
		return list(limited(data, r.Form), r.URL.Path), nil
	default:
		return nil, nil
	}
}

func (s *Server) customerResource(r *http.Request) (any, error) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/customers/"), "/")
	id := parts[0]
	c := s.customers[id]
	if c == nil {
		return nil, requestError("No such customer: " + id)
	}
	if len(parts) == 2 && parts[1] == "balance_transactions" && r.Method == http.MethodPost {
		return s.balanceTransaction(c, r.Form)
	}
	if len(parts) != 1 {
		return nil, nil
	}
	switch r.Method {
	case http.MethodGet:
		return s.customer(id, expanded(r.Form, "subscriptions")), nil
	case http.MethodDelete:
		delete(s.customers, id)
		for _, sub := range s.subscriptions {
			if sub.Customer == id {
				sub.Status = subscriptionCanceled
			}
		}
		return object{"id": id, objectField: customerResourceName, "deleted": true}, nil
	default:
		return nil, nil
	}
}

func (s *Server) balanceTransaction(c *customer, f url.Values) (object, error) {
	amount, err := strconv.ParseInt(f.Get("amount"), 10, 64)
	if err != nil || f.Get("currency") == "" {
		return nil, requestError("Balance transaction requires amount and currency")
	}
	c.Balance += amount
	return object{
		"id": s.id(
			"cbtxn",
		),
		objectField:          "customer_balance_transaction",
		customerResourceName: c.ID,
		"amount":             amount,
		"currency":           f.Get("currency"),
		"ending_balance":     c.Balance,
	}, nil
}

func (s *Server) subscriptionResource(r *http.Request) (any, error) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")
	sub := s.subscriptions[id]
	if sub == nil {
		return nil, requestError("No such subscription: " + id)
	}
	switch r.Method {
	case http.MethodGet:
		return sub, nil
	case http.MethodDelete:
		sub.Status = subscriptionCanceled
		return sub, nil
	case http.MethodPost:
		if err := s.updateSubscription(
			sub,
			r.Form.Get("items[0][id]"),
			r.Form.Get("items[0][price]"),
		); err != nil {
			return nil, err
		}
		if r.Form.Get("payment_behavior") == "default_incomplete" &&
			r.Form.Get("proration_behavior") == "always_invoice" {
			sub.Status = "past_due"
		}
		return sub, nil
	default:
		return nil, nil
	}
}

func (s *Server) createCheckout(f url.Values) (object, error) {
	customerID := f.Get(customerResourceName)
	if s.customers[customerID] == nil {
		return nil, requestError("No such customer: " + customerID)
	}
	p := s.prices[f.Get("line_items[0][price]")]
	if p == nil {
		return nil, requestError("Unknown Checkout price")
	}
	if f.Get("mode") != subscriptionResourceName || f.Get("success_url") == "" ||
		f.Get("cancel_url") == "" ||
		f.Get("line_items[0][quantity]") != "1" {
		return nil, requestError("Checkout requires subscription mode, return URLs and one item")
	}
	id := s.id("cs")
	c := &checkout{
		ID:            id,
		CustomerID:    customerID,
		Status:        "open",
		PaymentStatus: "unpaid",
		PaymentMethodCollection: f.Get(
			"payment_method_collection",
		),
		Metadata: metadata(f, "metadata"),
		SubscriptionMetadata: metadata(
			f,
			"subscription_data[metadata]",
		),
		ClientReferenceID: f.Get("client_reference_id"),
		SuccessURL: f.Get(
			"success_url",
		),
		CancelURL: f.Get("cancel_url"),
		URL:       s.config.Origin + "/checkout/" + id,
		Item:      checkoutItem{ID: s.id("li"), Object: "item", Price: p, Quantity: 1},
	}
	s.checkouts[id] = c
	return c.resource(), nil
}

func (s *Server) checkoutResource(r *http.Request) (object, error) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/")
	session := s.checkouts[id]
	if session == nil {
		return nil, requestError("No such Checkout Session: " + id)
	}
	result := session.resource()
	if expanded(r.Form, customerResourceName) {
		result[customerResourceName] = s.customer(session.CustomerID, false)
	}
	if expanded(r.Form, subscriptionResourceName) && session.SubscriptionID != "" {
		result[subscriptionResourceName] = s.subscriptions[session.SubscriptionID]
	}
	return result, nil
}

func (s *Server) createPortal(f url.Values) (object, error) {
	customerID := f.Get(customerResourceName)
	if s.customers[customerID] == nil {
		return nil, requestError("Unknown Portal customer")
	}
	id := s.id("bps")
	p := &portal{
		ID:         id,
		CustomerID: customerID,
		ReturnURL:  f.Get("return_url"),
		URL:        s.config.Origin + "/portal/" + id,
	}
	if f.Get("flow_data[type]") == subscriptionUpdateConfirm {
		flow, err := s.portalFlow(f, customerID)
		if err != nil {
			return nil, err
		}
		p.Flow = flow
	}
	s.portals[id] = p
	result := p.resource()
	if _, err := s.event("billing_portal.session.created", result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Server) portalFlow(f url.Values, customerID string) (*portalFlow, error) {
	sub := s.subscriptions[f.Get("flow_data[subscription_update_confirm][subscription]")]
	priceID := f.Get("flow_data[subscription_update_confirm][items][0][price]")
	if sub == nil || sub.Customer != customerID || s.prices[priceID] == nil {
		return nil, requestError("Unknown Portal subscription or price")
	}
	itemID := f.Get("flow_data[subscription_update_confirm][items][0][id]")
	returnURL := f.Get("flow_data[after_completion][redirect][return_url]")
	if len(sub.Items.Data) != 1 || sub.Items.Data[0].ID != itemID ||
		f.Get("flow_data[subscription_update_confirm][items][0][quantity]") != "1" ||
		f.Get("flow_data[after_completion][type]") != completionRedirect || returnURL == "" {
		return nil, requestError(
			"Portal confirmation requires its subscription item, quantity one and a completion redirect",
		)
	}
	return &portalFlow{
		Type: subscriptionUpdateConfirm,
		AfterCompletion: afterCompletion{
			Type:     completionRedirect,
			Redirect: redirect{ReturnURL: returnURL},
		},
		Update: subscriptionUpdate{
			Subscription: sub.ID,
			Items:        []updateItem{{ID: itemID, Price: priceID, Quantity: 1}},
		},
	}, nil
}

func (s *Server) listEvents(f url.Values) resourceList[*event] {
	var data []*event
	for _, e := range slices.Backward(s.events) {
		if f.Get("type") == "" || e.Type == f.Get("type") {
			data = append(data, e)
		}
	}
	return list(limited(data, f), "/v1/events")
}

func (s *Server) createSubscription(
	customerID, priceID string,
	meta object,
	behavior string,
) (*subscription, error) {
	c, p := s.customers[customerID], s.prices[priceID]
	if c == nil || p == nil {
		return nil, requestError("Subscription requires an existing customer and price")
	}
	outstanding := c.Balance + p.Amount
	if outstanding > 0 && c.InvoiceSettings.PaymentMethod == nil &&
		behavior == "error_if_incomplete" {
		return nil, requestError("Subscription requires a payment method or sufficient credit")
	}
	status := subscriptionActive
	if outstanding > 0 && c.InvoiceSettings.PaymentMethod == nil {
		status = "incomplete"
	} else {
		c.Balance = min(int64(0), outstanding)
	}
	sub := &subscription{
		ID:       s.id("sub"),
		Object:   subscriptionResourceName,
		Customer: customerID,
		Status:   status,
		Metadata: meta,
		Items: list(
			[]*subscriptionItem{{
				ID: s.id("si"), Object: "subscription_item", Price: p, Quantity: 1,
				PeriodStart: time.Now().Unix(), PeriodEnd: periodEnd(p),
			}},
			"/v1/subscription_items",
		),
	}
	s.subscriptions[sub.ID] = sub
	return sub, nil
}

func (s *Server) updateSubscription(sub *subscription, itemID, priceID string) error {
	p := s.prices[priceID]
	if p == nil || len(sub.Items.Data) != 1 || sub.Items.Data[0].ID != itemID {
		return requestError("Subscription update requires its existing item and a known price")
	}
	sub.Items.Data[0].Price = p
	sub.Items.Data[0].PeriodEnd = periodEnd(p)
	return nil
}

var hostedPage = template.Must(
	template.New("billing").Parse(`<!doctype html><html lang="en"><head>` +
		`<meta charset="utf-8"><title>{{.Title}}</title></head><body><main><h1>{{.Title}}</h1>` +
		`<form method="post">{{if .Card}}<label>Card number<input name="card" required autocomplete="cc-number">` +
		`</label>{{end}}<button type="submit">{{.Button}}</button></form>` +
		`<a href="{{.Cancel}}">Cancel</a></main></body></html>`),
)

type hostedForm struct {
	Title  string
	Button string
	Cancel string
	Card   bool
}

func (s *Server) hosted(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		invalid(w, http.StatusBadRequest, "Invalid hosted form")
		return
	}
	s.mu.Lock()
	form, done, status, err := s.hostedSession(r)
	s.mu.Unlock()
	if err != nil {
		invalid(w, status, err.Error())
		return
	}
	if status == http.StatusNotFound {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := hostedPage.Execute(w, form); err != nil {
			slog.Error("write Stripe hosted page", "error", err)
		}
		return
	}
	payload, err := json.Marshal(done.Event)
	if err != nil {
		invalid(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.deliver(r.Context(), payload); err != nil {
		invalid(w, http.StatusBadGateway, err.Error())
		return
	}
	http.Redirect(w, r, done.URL, http.StatusSeeOther)
}

// hostedSession reads or completes a session with the server lock held. Delivery happens after unlocking.
func (s *Server) hostedSession(r *http.Request) (hostedForm, *completion, int, error) {
	if id, checkout := strings.CutPrefix(r.URL.Path, "/checkout/"); checkout {
		session := s.checkouts[id]
		if session == nil {
			return hostedForm{}, nil, http.StatusNotFound, nil
		}
		if r.Method == http.MethodGet {
			c := s.customers[session.CustomerID]
			if c == nil {
				return hostedForm{}, nil, http.StatusNotFound, nil
			}
			return hostedForm{
				Title:  "Subscribe",
				Button: "Subscribe",
				Cancel: session.CancelURL,
				Card: c.Balance+session.Item.Price.Amount > 0 &&
					c.InvoiceSettings.PaymentMethod == nil,
			}, nil, http.StatusOK, nil
		}
		done, err := s.completeCheckout(session, r.Form)
		return hostedForm{}, done, http.StatusBadRequest, err
	}
	session := s.portals[strings.TrimPrefix(r.URL.Path, "/portal/")]
	if session == nil {
		return hostedForm{}, nil, http.StatusNotFound, nil
	}
	if r.Method == http.MethodGet {
		return hostedForm{
			Title:  "Confirm subscription update",
			Button: "Confirm",
			Cancel: session.ReturnURL,
		}, nil, http.StatusOK, nil
	}
	done, err := s.completePortal(session)
	return hostedForm{}, done, http.StatusBadRequest, err
}

func (s *Server) completeCheckout(session *checkout, f url.Values) (*completion, error) {
	if session.Completion != nil {
		return session.Completion, nil
	}
	c := s.customers[session.CustomerID]
	if c == nil {
		return nil, requestError("Checkout customer was deleted")
	}
	if f.Get("card") != "" {
		pm := "pm_card_visa"
		c.InvoiceSettings.PaymentMethod = &pm
	}
	sub, err := s.createSubscription(
		c.ID,
		session.Item.Price.ID,
		session.SubscriptionMetadata,
		"error_if_incomplete",
	)
	if err != nil {
		return nil, err
	}
	session.SubscriptionID = sub.ID
	session.Status = "complete"
	session.PaymentStatus = "paid"
	e, err := s.event("customer.subscription.created", sub)
	if err != nil {
		return nil, err
	}
	session.Completion = &completion{Event: e, URL: session.SuccessURL}
	return session.Completion, nil
}

func (s *Server) completePortal(session *portal) (*completion, error) {
	if session.Completion != nil {
		return session.Completion, nil
	}
	flow := session.Flow
	if flow == nil {
		return nil, requestError("Portal session has no update flow")
	}
	sub := s.subscriptions[flow.Update.Subscription]
	if sub == nil || len(flow.Update.Items) != 1 {
		return nil, requestError("Invalid Portal update")
	}
	item := flow.Update.Items[0]
	if err := s.updateSubscription(sub, item.ID, item.Price); err != nil {
		return nil, requestError("Invalid Portal update")
	}
	sub.Status = subscriptionActive
	e, err := s.event("customer.subscription.updated", sub)
	if err != nil {
		return nil, err
	}
	session.Completion = &completion{Event: e, URL: flow.AfterCompletion.Redirect.ReturnURL}
	return session.Completion, nil
}

func (s *Server) deliver(ctx context.Context, payload []byte) error {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(s.config.WebhookSigningSecret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(payload)
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		s.config.WebhookURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Stripe-Signature", "t="+timestamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
	response, err := s.config.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("deliver Stripe webhook: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxWebhookResponseBytes))
	if err != nil {
		return err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return requestError(
			fmt.Sprintf("Stripe webhook returned %d: %s", response.StatusCode, body),
		)
	}
	return nil
}

func limited[T any](data []T, f url.Values) []T {
	if limit, _ := strconv.Atoi(f.Get("limit")); limit > 0 && len(data) > limit {
		return data[:limit]
	}
	return data
}

func expanded(form url.Values, resource string) bool {
	for key, values := range form {
		if strings.HasPrefix(key, "expand[") && slices.Contains(values, resource) {
			return true
		}
	}
	return false
}

func metadata(form url.Values, prefix string) object {
	result := object{}
	for key, values := range form {
		if strings.HasPrefix(key, prefix+"[") && strings.HasSuffix(key, "]") && len(values) > 0 {
			result[strings.TrimSuffix(strings.TrimPrefix(key, prefix+"["), "]")] = values[0]
		}
	}
	return result
}

func writeJSONBytes(w http.ResponseWriter, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(data); err != nil {
		slog.Error("write Stripe mock response", "error", err)
	}
}

func invalid(w http.ResponseWriter, status int, message string) {
	// Only strings and maps of strings are encoded; a transport error is reported locally.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).
		Encode(object{"error": object{"type": "invalid_request_error", "message": message}}); err != nil {
		slog.Error("write Stripe mock error", "error", err)
	}
}
