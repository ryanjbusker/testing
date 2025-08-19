# Voice Add-Ons Setup Guide

This guide explains how to set up voice quality add-ons as separate Stripe products for BMM Translation.

## Overview

Voice add-ons are implemented as fixed-price subscription items that are added to the main plan subscription. The add-on price is calculated based on the plan hours (e.g., 8-hour plan with premium voice = 8 × $15 = $120 extra).

## Voice Options

1. **Standard Voice (Polly)** - Included with all plans
2. **Premium Voice (ElevenLabs)** - Fixed add-on based on plan hours
3. **Custom Voice (ElevenLabs + Voice Cloning)** - Fixed add-on based on plan hours

## Stripe Setup

### 1. Create Voice Add-On Products

In your Stripe Dashboard, create these products with **fixed recurring prices**:

#### Premium Voice (ElevenLabs) Products
- **Premium Voice - 5 Hours Monthly**: $75/month (5 × $15)
- **Premium Voice - 8 Hours Monthly**: $120/month (8 × $15)
- **Premium Voice - 13 Hours Monthly**: $195/month (13 × $15)
- **Premium Voice - 50 Hours Yearly**: $750/year (50 × $15)
- **Premium Voice - 100 Hours Yearly**: $1,500/year (100 × $15)
- **Premium Voice - 150 Hours Yearly**: $2,250/year (150 × $15)
- **Premium Voice - 8 Hours Yearly**: $120/year (8 × $15)

#### Custom Voice (ElevenLabs + Voice Cloning) Products
- **Custom Voice - 5 Hours Monthly**: $100/month (5 × $20)
- **Custom Voice - 8 Hours Monthly**: $160/month (8 × $20)
- **Custom Voice - 13 Hours Monthly**: $260/month (13 × $20)
- **Custom Voice - 50 Hours Yearly**: $1,000/year (50 × $20)
- **Custom Voice - 100 Hours Yearly**: $2,000/year (100 × $20)
- **Custom Voice - 150 Hours Yearly**: $3,000/year (150 × $20)
- **Custom Voice - 8 Hours Yearly**: $160/year (8 × $20)

### 2. Create Fixed Recurring Prices

For each voice product, create a **fixed recurring price**:

1. Go to the product page
2. Click "Add pricing"
3. Select "Standard pricing"
4. Set the price to the calculated amount (hours × rate)
5. Set billing period to match the corresponding main plan
6. Copy the price IDs

### 3. Environment Variables

Add these environment variables to your `.env` file:

```bash
# Premium Voice (ElevenLabs) Add-On Price IDs
STRIPE_VOICE_ELEVENLABS_5H_PRICE_ID=price_...
STRIPE_VOICE_ELEVENLABS_8H_PRICE_ID=price_...
STRIPE_VOICE_ELEVENLABS_13H_PRICE_ID=price_...
STRIPE_VOICE_ELEVENLABS_50H_PRICE_ID=price_...
STRIPE_VOICE_ELEVENLABS_100H_PRICE_ID=price_...
STRIPE_VOICE_ELEVENLABS_150H_PRICE_ID=price_...
STRIPE_VOICE_ELEVENLABS_8H_YEARLY_PRICE_ID=price_...

# Custom Voice (ElevenLabs + Voice Cloning) Add-On Price IDs
STRIPE_VOICE_CUSTOM_5H_PRICE_ID=price_...
STRIPE_VOICE_CUSTOM_8H_PRICE_ID=price_...
STRIPE_VOICE_CUSTOM_13H_PRICE_ID=price_...
STRIPE_VOICE_CUSTOM_50H_PRICE_ID=price_...
STRIPE_VOICE_CUSTOM_100H_PRICE_ID=price_...
STRIPE_VOICE_CUSTOM_150H_PRICE_ID=price_...
STRIPE_VOICE_CUSTOM_8H_YEARLY_PRICE_ID=price_...
```

## Database Changes

The `speakers` table now includes a `voice_preference` column:

```sql
ALTER TABLE speakers ADD COLUMN voice_preference TEXT DEFAULT 'polly';
```

This column stores the user's selected voice option:
- `polly` - Standard voice (default)
- `elevenlabs` - Premium voice
- `custom` - Custom voice with cloning

## How It Works

### Subscription Creation

When a user registers and selects a voice add-on:

1. The system creates a single Stripe subscription
2. The subscription contains multiple items:
   - Main plan item (e.g., "Starter Monthly - 8 Hours")
   - Voice add-on item (e.g., "Premium Voice - 8 Hours Monthly")
3. Both items are billed together on the same schedule
4. The voice add-on price is fixed based on the plan hours

### Pricing Calculation

Voice add-ons are priced based on the plan hours:
- **Premium Voice**: Plan hours × $15
- **Custom Voice**: Plan hours × $20

### Webhook Handling

The existing webhook handler automatically handles:
- Subscription status updates for both main plan and add-ons
- Payment success/failure events
- Subscription cancellations

## Testing

### Test Scenarios

1. **Standard Voice Only:**
   - Select any plan + "Standard Voice"
   - Should create subscription with only main plan item

2. **Premium Voice Add-On:**
   - Select "Starter Monthly (8h)" + "Premium Voice"
   - Should create subscription with main plan + "Premium Voice - 8 Hours Monthly" ($120)
   - Total: $400 + $120 = $520/month

3. **Custom Voice Add-On:**
   - Select "Professional Yearly (100h)" + "Custom Voice"
   - Should create subscription with main plan + "Custom Voice - 100 Hours Yearly" ($2,000)
   - Total: $4,000 + $2,000 = $6,000/year

### Test Cards

Use Stripe's test cards:
- **Success:** 4242 4242 4242 4242
- **Decline:** 4000 0000 0000 0002
- **Insufficient Funds:** 4000 0000 0000 9995

## Pricing Examples

### Monthly Plans with Voice Add-Ons

| Plan | Base Cost | Premium Voice | Custom Voice | Total |
|------|-----------|---------------|--------------|-------|
| Starter (8h) | $400 | +$120 | +$160 | $520/$560 |
| Professional (100h) | $4,000 | +$1,500 | +$2,000 | $5,500/$6,000 |
| Enterprise (150h) | $4,500 | +$2,250 | +$3,000 | $6,750/$7,500 |

### Yearly Plans with Voice Add-Ons

| Plan | Base Cost | Premium Voice | Custom Voice | Total |
|------|-----------|---------------|--------------|-------|
| Starter (50h) | $2,500 | +$750 | +$1,000 | $3,250/$3,500 |
| Professional (100h) | $4,000 | +$1,500 | +$2,000 | $5,500/$6,000 |
| Enterprise (150h) | $4,500 | +$2,250 | +$3,000 | $6,750/$7,500 |

## Troubleshooting

### Common Issues

1. **Voice add-on not added to subscription:**
   - Check that the voice price ID environment variables are set
   - Verify the price IDs exist in Stripe
   - Check server logs for price ID validation errors

2. **Incorrect billing amounts:**
   - Verify fixed pricing is configured correctly
   - Check that the price matches the calculated amount (hours × rate)
   - Ensure billing periods match between main plan and add-ons

3. **Webhook errors:**
   - Verify webhook endpoint is configured for all subscription events
   - Check webhook secret is correct
   - Monitor webhook delivery logs in Stripe dashboard

### Logs to Monitor

The application logs the following for voice add-ons:
- Voice price ID configuration status for all plan combinations
- Voice add-on selection during registration
- Subscription creation with multiple items
- Voice preference storage in database

## Security Notes

- Voice preferences are stored in the database and linked to user accounts
- Voice add-ons are tied to the main subscription and cannot be purchased separately
- All billing is handled securely through Stripe
- Voice cloning data is stored securely and can be deleted on account cancellation 