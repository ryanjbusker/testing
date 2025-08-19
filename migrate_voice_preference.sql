-- Migration script to add voice_preference column to speakers table
-- Run this script if you have an existing speakers table without the voice_preference column

-- Add the voice_preference column with default value
ALTER TABLE speakers ADD COLUMN IF NOT EXISTS voice_preference TEXT DEFAULT 'polly';

-- Update any existing records that might have NULL values
UPDATE speakers SET voice_preference = 'polly' WHERE voice_preference IS NULL;

-- Verify the migration
SELECT COUNT(*) as total_speakers, 
       COUNT(CASE WHEN voice_preference = 'polly' THEN 1 END) as polly_users,
       COUNT(CASE WHEN voice_preference = 'elevenlabs' THEN 1 END) as elevenlabs_users,
       COUNT(CASE WHEN voice_preference = 'custom' THEN 1 END) as custom_users
FROM speakers; 