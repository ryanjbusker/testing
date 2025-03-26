#!/bin/bash

# Build the Go application
go build -o main

# Make sure the main binary is executable
chmod +x main

# Verify templates exist and are different
echo "Verifying template files:"
for file in templates/*.html; do
    echo "Checking $file:"
    head -n 1 "$file"
done 