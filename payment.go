package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/customer"
	"github.com/stripe/stripe-go/v78/subscription"
	"github.com/stripe/stripe-go/v78/usagerecord"
	"github.com/stripe/stripe-go/v78/webhook"
)

// PaymentService handles all payment-related operations
type PaymentService struct {
	db *sql.DB
}

// NewPaymentService creates a new PaymentService instance
func NewPaymentService(db *sql.DB) *PaymentService {
	return &PaymentService{db: db}
}

// CreateStripeCustomer creates a new Stripe customer for a speaker
func (ps *PaymentService) CreateStripeCustomer(speaker *Speaker) error {
	params := &stripe.CustomerParams{
		Email: stripe.String(speaker.Email),
		Name:  stripe.String(speaker.Name),
		Metadata: map[string]string{
			"speaker_code": speaker.SpeakerCode,
			"google_id":    speaker.GoogleID,
		},
	}

	customer, err := customer.New(params)
	if err != nil {
		return fmt.Errorf("error creating Stripe customer: %v", err)
	}

	// Update speaker with Stripe customer ID
	_, err = ps.db.Exec(`
        UPDATE speakers 
        SET stripe_customer_id = $1 
        WHERE id = $2`,
		customer.ID,
		speaker.ID,
	)
	if err != nil {
		return fmt.Errorf("error updating speaker with Stripe customer ID: %v", err)
	}

	speaker.StripeCustomerID = customer.ID
	return nil
}

// CreateMeteredSubscription creates a metered subscription for a speaker
func (ps *PaymentService) CreateMeteredSubscription(speaker *Speaker) error {
	// Create Stripe customer if not exists
	if speaker.StripeCustomerID == "" {
		if err := ps.CreateStripeCustomer(speaker); err != nil {
			return fmt.Errorf("error creating Stripe customer: %v", err)
		}
	}

	// Create the subscription
	params := &stripe.SubscriptionParams{
		Customer: stripe.String(speaker.StripeCustomerID),
		Items: []*stripe.SubscriptionItemsParams{
			{
				Price: stripe.String(os.Getenv("STRIPE_PRICE_ID")),
			},
		},
		PaymentBehavior: stripe.String("default_incomplete"),
		PaymentSettings: &stripe.SubscriptionPaymentSettingsParams{
			PaymentMethodTypes: []*string{
				stripe.String("card"),
			},
		},
		Expand: []*string{
			stripe.String("latest_invoice.payment_intent"),
		},
	}

	subscription, err := subscription.New(params)
	if err != nil {
		return fmt.Errorf("error creating subscription: %v", err)
	}

	// Update speaker with subscription ID
	_, err = ps.db.Exec(`
        UPDATE speakers 
        SET subscription_id = $1 
        WHERE id = $2`,
		subscription.ID,
		speaker.ID,
	)
	if err != nil {
		return fmt.Errorf("error updating speaker with subscription ID: %v", err)
	}

	speaker.SubscriptionID = subscription.ID
	return nil
}

// ReportUsageToStripe reports the usage to Stripe based on speaking sessions
func (ps *PaymentService) ReportUsageToStripe(speakerID string) error {
	// Get the speaker's subscription ID
	var subscriptionID string
	err := ps.db.QueryRow("SELECT subscription_id FROM speakers WHERE google_id = $1", speakerID).Scan(&subscriptionID)
	if err != nil {
		return fmt.Errorf("error getting subscription ID: %v", err)
	}

	if subscriptionID == "" {
		return nil // No subscription, no need to report usage
	}

	// Get total minutes from speaking sessions for the current billing period
	var totalMinutes int64
	err = ps.db.QueryRow(`
        SELECT COALESCE(SUM(EXTRACT(EPOCH FROM total_duration) / 60), 0)
        FROM speaking_sessions
        WHERE speaker_id = (SELECT id FROM speakers WHERE google_id = $1)
        AND session_end >= NOW() - INTERVAL '1 month'
    `, speakerID).Scan(&totalMinutes)

	if err != nil {
		return fmt.Errorf("error calculating usage: %v", err)
	}

	if totalMinutes > 0 {
		// Report usage to Stripe
		params := &stripe.UsageRecordParams{
			Quantity:         &totalMinutes,
			SubscriptionItem: stripe.String(subscriptionID),
			Timestamp:        stripe.Int64(time.Now().Unix()),
			Action:           stripe.String("set"),
		}

		_, err := usagerecord.New(params)
		if err != nil {
			return fmt.Errorf("error reporting usage to Stripe: %v", err)
		}
	}

	return nil
}

// HandleStripeWebhook handles incoming webhook events from Stripe
func (ps *PaymentService) HandleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	const MaxBodyBytes = int64(65536)
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		fmt.Printf("Error reading request body: %v\n", err)
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}

	event := stripe.Event{}
	if err := json.Unmarshal(payload, &event); err != nil {
		fmt.Printf("Error parsing webhook JSON: %v\n", err)
		http.Error(w, "Error parsing webhook JSON", http.StatusBadRequest)
		return
	}

	// Verify the event signature
	endpointSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if endpointSecret != "" {
		signature := r.Header.Get("Stripe-Signature")
		event, err = webhook.ConstructEvent(payload, signature, endpointSecret)
		if err != nil {
			fmt.Printf("Error verifying webhook signature: %v\n", err)
			http.Error(w, "Error verifying webhook signature", http.StatusBadRequest)
			return
		}
	}

	// Handle the event
	switch event.Type {
	case "customer.subscription.created":
		var subscription stripe.Subscription
		err := json.Unmarshal(event.Data.Raw, &subscription)
		if err != nil {
			fmt.Printf("Error parsing subscription: %v\n", err)
			http.Error(w, "Error parsing subscription", http.StatusBadRequest)
			return
		}
		// Update speaker's subscription status
		_, err = ps.db.Exec(`
            UPDATE speakers 
            SET subscription_id = $1, payment_status = 'active'
            WHERE stripe_customer_id = $2`,
			subscription.ID,
			subscription.Customer.ID,
		)
		if err != nil {
			fmt.Printf("Error updating speaker subscription: %v\n", err)
			http.Error(w, "Error updating speaker subscription", http.StatusInternalServerError)
			return
		}

	case "customer.subscription.updated":
		var subscription stripe.Subscription
		err := json.Unmarshal(event.Data.Raw, &subscription)
		if err != nil {
			fmt.Printf("Error parsing subscription: %v\n", err)
			http.Error(w, "Error parsing subscription", http.StatusBadRequest)
			return
		}
		// Update speaker's subscription status
		status := "active"
		if subscription.Status == "canceled" || subscription.Status == "unpaid" {
			status = "inactive"
		}
		_, err = ps.db.Exec(`
            UPDATE speakers 
            SET payment_status = $1
            WHERE subscription_id = $2`,
			status,
			subscription.ID,
		)
		if err != nil {
			fmt.Printf("Error updating speaker payment status: %v\n", err)
			http.Error(w, "Error updating speaker payment status", http.StatusInternalServerError)
			return
		}

	case "invoice.payment_succeeded":
		var invoice stripe.Invoice
		err := json.Unmarshal(event.Data.Raw, &invoice)
		if err != nil {
			fmt.Printf("Error parsing invoice: %v\n", err)
			http.Error(w, "Error parsing invoice", http.StatusBadRequest)
			return
		}
		// Update speaker's payment status
		_, err = ps.db.Exec(`
            UPDATE speakers 
            SET payment_status = 'active'
            WHERE stripe_customer_id = $1`,
			invoice.Customer.ID,
		)
		if err != nil {
			fmt.Printf("Error updating speaker payment status: %v\n", err)
			http.Error(w, "Error updating speaker payment status", http.StatusInternalServerError)
			return
		}

	case "invoice.payment_failed":
		var invoice stripe.Invoice
		err := json.Unmarshal(event.Data.Raw, &invoice)
		if err != nil {
			fmt.Printf("Error parsing invoice: %v\n", err)
			http.Error(w, "Error parsing invoice", http.StatusBadRequest)
			return
		}
		// Update speaker's payment status
		_, err = ps.db.Exec(`
            UPDATE speakers 
            SET payment_status = 'inactive'
            WHERE stripe_customer_id = $1`,
			invoice.Customer.ID,
		)
		if err != nil {
			fmt.Printf("Error updating speaker payment status: %v\n", err)
			http.Error(w, "Error updating speaker payment status", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}
