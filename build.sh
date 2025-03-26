#!/bin/bash

# Build the Go application
go build -o main

# Make sure the main binary is executable
chmod +x main

# Verify templates exist and are different
echo "Verifying template files:"
for file in templates/*.html; do
    if [ -f "$file" ]; then
        echo "Found $file:"
        head -n 1 "$file"
    else
        echo "ERROR: $file not found!"
    fi
done

# Verify static files
echo "Verifying static files:"
if [ -f "static/styles.css" ]; then
    echo "Found static/styles.css"
else
    echo "ERROR: static/styles.css not found!"
fi 