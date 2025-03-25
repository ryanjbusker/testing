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