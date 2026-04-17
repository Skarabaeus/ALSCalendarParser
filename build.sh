#!/bin/bash
set -e

# Build the Go binary for Linux
echo "Building Go binary..."
GOOS=linux GOARCH=amd64 go build -o bootstrap main.go

# Create the deployment package
echo "Creating deployment package..."
zip function.zip bootstrap

# Update the Lambda function
echo "Updating Lambda function..."
aws lambda update-function-code --function-name ALSCalendarParser --zip-file fileb://function.zip --region eu-central-1 --profile als-calendar

echo "Build and deployment complete!" 