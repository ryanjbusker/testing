package models

import (
	"database/sql"
	"time"
)

// User represents a user in the system
type User struct {
	ID            int64          `json:"id"`
	GoogleSub     string         `json:"google_sub"` // Google's unique identifier
	Email         string         `json:"email"`
	Name          string         `json:"name"`
	IsSpeaker     bool           `json:"is_speaker"`
	Subscription  string         `json:"subscription"`   // e.g., "free", "premium", "enterprise"
	PaymentStatus string         `json:"payment_status"` // e.g., "active", "past_due", "canceled"
	StripeID      sql.NullString `json:"stripe_id"`      // Stripe customer ID
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// CreateUsersTable creates the users table if it doesn't exist
func CreateUsersTable(db *sql.DB) error {
	query := `
    CREATE TABLE IF NOT EXISTS users (
        id SERIAL PRIMARY KEY,
        google_sub VARCHAR(255) UNIQUE NOT NULL,
        email VARCHAR(255) UNIQUE NOT NULL,
        name VARCHAR(255) NOT NULL,
        is_speaker BOOLEAN DEFAULT FALSE,
        subscription VARCHAR(50) DEFAULT 'free',
        payment_status VARCHAR(50) DEFAULT 'inactive',
        stripe_id VARCHAR(255),
        created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
        updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
    );
    
    CREATE INDEX IF NOT EXISTS idx_users_google_sub ON users(google_sub);
    CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
    CREATE INDEX IF NOT EXISTS idx_users_stripe_id ON users(stripe_id);
    `

	_, err := db.Exec(query)
	return err
}

// CreateOrUpdateUser creates a new user or updates an existing one
func CreateOrUpdateUser(db *sql.DB, user *User) error {
	query := `
    INSERT INTO users (google_sub, email, name, created_at, updated_at)
    VALUES ($1, $2, $3, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
    ON CONFLICT (google_sub) 
    DO UPDATE SET 
        email = EXCLUDED.email,
        name = EXCLUDED.name,
        updated_at = CURRENT_TIMESTAMP
    RETURNING id, created_at, updated_at;
    `

	return db.QueryRow(query, user.GoogleSub, user.Email, user.Name).Scan(
		&user.ID, &user.CreatedAt, &user.UpdatedAt,
	)
}

// GetUserByGoogleSub retrieves a user by their Google sub ID
func GetUserByGoogleSub(db *sql.DB, googleSub string) (*User, error) {
	user := &User{}
	query := `
    SELECT id, google_sub, email, name, is_speaker, subscription, 
           payment_status, stripe_id, created_at, updated_at
    FROM users
    WHERE google_sub = $1;
    `

	err := db.QueryRow(query, googleSub).Scan(
		&user.ID, &user.GoogleSub, &user.Email, &user.Name,
		&user.IsSpeaker, &user.Subscription, &user.PaymentStatus,
		&user.StripeID, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return user, nil
}

// UpdateUserSubscription updates a user's subscription and payment status
func UpdateUserSubscription(db *sql.DB, googleSub, subscription, paymentStatus string) error {
	query := `
    UPDATE users
    SET subscription = $1,
        payment_status = $2,
        updated_at = CURRENT_TIMESTAMP
    WHERE google_sub = $3;
    `

	_, err := db.Exec(query, subscription, paymentStatus, googleSub)
	return err
}

// UpdateStripeID updates a user's Stripe customer ID
func UpdateStripeID(db *sql.DB, googleSub, stripeID string) error {
	query := `
    UPDATE users
    SET stripe_id = $1,
        updated_at = CURRENT_TIMESTAMP
    WHERE google_sub = $2;
    `

	_, err := db.Exec(query, stripeID, googleSub)
	return err
}

// UpdateSpeakerStatus updates a user's speaker status
func UpdateSpeakerStatus(db *sql.DB, googleSub string, isSpeaker bool) error {
	query := `
    UPDATE users
    SET is_speaker = $1,
        updated_at = CURRENT_TIMESTAMP
    WHERE google_sub = $2;
    `

	_, err := db.Exec(query, isSpeaker, googleSub)
	return err
}
