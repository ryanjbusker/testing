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
STRIPE_MONTHLY_5_PRICE_ID=price_...
STRIPE_MONTHLY_8_PRICE_ID=price_...
STRIPE_MONTHLY_13_PRICE_ID=price_...
STRIPE_YEARLY_50_PRICE_ID=price_...
STRIPE_YEARLY_100_PRICE_ID=price_...
STRIPE_YEARLY_150_PRICE_ID=price_...

# Voice Add-On Price IDs
STRIPE_VOICE_ELEVENLABS_PRICE_ID=price_...
STRIPE_VOICE_CUSTOM_PRICE_ID=price_...
```

## Stripe Setup

1. **Create Main Plan Products and Prices in Stripe Dashboard:**
   - Go to your Stripe Dashboard
   - Navigate to Products
   - Create the main plan products:
     - Monthly 5 Hours Plan
     - Monthly 8 Hours Plan  
     - Monthly 13 Hours Plan
     - Yearly 50 Hours Plan
     - Yearly 100 Hours Plan
     - Yearly 150 Hours Plan
   - For each product, create a recurring price
   - Copy the price IDs to your environment variables

2. **Create Voice Add-On Products:**
   - Create two additional products for voice upgrades:
     - Premium Voice (ElevenLabs) - $15/hour usage-based
     - Custom Voice (ElevenLabs + Voice Cloning) - $20/hour usage-based
   - Set up usage-based pricing for both voice products
   - Copy the voice price IDs to your environment variables
   - See `VOICE_ADDONS_SETUP.md` for detailed voice add-on setup instructions

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
    stripe_customer_id TEXT,
    plan_name TEXT,
    voice_preference TEXT DEFAULT 'polly'
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

### Main Plans
- **Starter Monthly (8h):** $400/month - $50/hour rate
- **Starter Yearly (50h):** $2,500/year - $50/hour rate  
- **Professional Yearly (100h):** $4,000/year - $40/hour rate
- **Enterprise Yearly (150h):** $4,500/year - $30/hour rate
- **Just in Case (8h/year):** $400/year - $50/hour rate

### Voice Add-Ons
- **Standard Voice (Polly):** Included with all plans
- **Premium Voice (ElevenLabs):** +$15/hour add-on
- **Custom Voice (ElevenLabs + Voice Cloning):** +$20/hour add-on

See `VOICE_ADDONS_SETUP.md` for detailed pricing examples and setup instructions.

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