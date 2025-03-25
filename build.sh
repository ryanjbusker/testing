#!/bin/bash

# Build the Go application
go build -o main

# Create necessary directories
mkdir -p static templates

# Copy files to their locations
cp -r templates/* templates/ 2>/dev/null || true
cp -r static/* static/ 2>/dev/null || true

# Make sure the main binary is executable
chmod +x main

# Verify templates exist
if [ ! -f "templates/index.html" ]; then
    echo "Error: templates/index.html not found"
    exit 1
fi

if [ ! -f "templates/speaker.html" ]; then
    echo "Error: templates/speaker.html not found"
    exit 1
fi

if [ ! -f "templates/audience.html" ]; then
    echo "Error: templates/audience.html not found"
    exit 1
fi 