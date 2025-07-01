# Join Page Setup Guide

This guide explains how to set up the new Join page with payment integration for BMM Translation.

## Features

The Join page includes:
- User registration form with personal information
- Auto-generated 5-digit speaker codes
- Payment plan selection (Basic, Pro, Enterprise)
- Stripe payment integration
- Database insertion for new speakers

## Required Environment Variables

Add these environment variables to your `.env` file:

```bash
# Stripe Configuration
STRIPE_SECRET_KEY=sk_test_...
STRIPE_PUBLISHABLE_KEY=pk_test_...
STRIPE_WEBHOOK_SECRET=whsec_...

# Stripe Price IDs for different plans
STRIPE_BASIC_PRICE_ID=price_...
STRIPE_PRO_PRICE_ID=price_...
STRIPE_ENTERPRISE_PRICE_ID=price_...
```

## Stripe Setup

1. **Create Products and Prices in Stripe Dashboard:**
   - Go to your Stripe Dashboard
   - Navigate to Products
   - Create three products:
     - Basic Plan ($9.99/month)
     - Pro Plan ($19.99/month)
     - Enterprise Plan ($49.99/month)
   - For each product, create a recurring price
   - Copy the price IDs to your environment variables

2. **Configure Webhooks:**
   - In Stripe Dashboard, go to Webhooks
   - Add endpoint: `https://yourdomain.com/webhook`
   - Select events:
     - `customer.subscription.created`
     - `customer.subscription.updated`
     - `invoice.payment_succeeded`
     - `invoice.payment_failed`
   - Copy the webhook secret to your environment variables

## Database Schema

The join functionality uses the existing `speakers` table and `user_info` table. Make sure these tables exist:

```sql
-- Speakers table (should already exist)
CREATE TABLE IF NOT EXISTS speakers (
    id SERIAL PRIMARY KEY,
    google_id TEXT UNIQUE,
    email TEXT UNIQUE NOT NULL,
    name TEXT,
    speaker_code TEXT UNIQUE NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    payment_status TEXT,
    subscription_id TEXT,
    stripe_customer_id TEXT
);

-- User info table (should already exist)
CREATE TABLE IF NOT EXISTS user_info (
    id SERIAL PRIMARY KEY,
    speaker_id INTEGER REFERENCES speakers(id),
    preferred_language TEXT,
    timezone TEXT,
    notification_preferences TEXT,
    last_login TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);
```

## Usage

1. **Access the Join page:** Navigate to `/join`
2. **Fill out the form:** Enter personal information and select a plan
3. **Complete payment:** Enter credit card information
4. **Get speaker code:** A unique 5-digit code is generated and displayed
5. **Start speaking:** Use the speaker code to access speaker features

## Payment Plans

- **Basic Plan ($9.99/month):** Up to 10 hours per month, basic support
- **Pro Plan ($19.99/month):** Up to 50 hours per month, priority support
- **Enterprise Plan ($49.99/month):** Unlimited hours, 24/7 support

## Security Notes

- All payment processing is handled securely through Stripe
- Speaker codes are unique and randomly generated
- User data is stored in the database with proper validation
- Payment status is tracked and updated via Stripe webhooks

## Testing

For testing, use Stripe's test mode:
- Test card numbers: 4242 4242 4242 4242
- Expiry: Any future date
- CVC: Any 3 digits

## Troubleshooting

1. **Payment fails:** Check Stripe logs and ensure price IDs are correct
2. **Speaker code generation fails:** Check database connectivity
3. **Webhook issues:** Verify webhook secret and endpoint URL
4. **Database errors:** Ensure all required tables exist with correct schema 