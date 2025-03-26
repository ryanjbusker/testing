#!/bin/bash

echo "Starting build process..."

# Ensure directories exist
mkdir -p templates static

# Build the Go application
echo "Building Go application..."
go build -o main

# Make sure the main binary is executable
chmod +x main

# Verify templates exist and show their contents
echo "Verifying template files..."
for template in index.html speaker.html audience.html; do
    if [ -f "templates/$template" ]; then
        echo "Found templates/$template:"
        head -n 5 "templates/$template"
    else
        echo "ERROR: templates/$template not found!"
        exit 1
    fi
done

# Verify static files
echo "Verifying static files..."
if [ -f "static/styles.css" ]; then
    echo "Found static/styles.css"
else
    echo "ERROR: static/styles.css not found!"
    exit 1
fi

echo "Build process completed successfully" 