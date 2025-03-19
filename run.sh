#!/bin/bash

# Invoke the Lambda function and save the response
echo "Invoking Lambda function..."
aws lambda invoke --function-name ALSCalendarParser --payload fileb://test-event.json response.json

# Display the response
echo -e "\nLambda function response:"
cat response.json | jq . 