#!/bin/bash

echo "🏪 Kopitiam Run - Starting Server..."
echo ""
echo "📦 Installing dependencies..."
go mod download

echo ""
echo "🚀 Starting application on http://localhost:8080"
echo ""
echo "Press Ctrl+C to stop the server"
echo ""

go run main.go