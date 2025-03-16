package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"golang.org/x/net/html"
)

const (
	url        = "https://als-usingen.de/kalender/"
	userAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15"
	tableName  = "ALSEvents"
	secretName = "prod/mailersend/apitoken"
)

// Event represents a calendar event with a date and description
type Event struct {
	EventDate        time.Time `json:"date"`
	EventDescription string    `json:"description"`
}

// DynamoDBEvent represents an event as stored in DynamoDB
type DynamoDBEvent struct {
	EventKey      string    `dynamodbav:"eventKey"`
	EventDate     time.Time `dynamodbav:"eventDate"`
	EventDesc     string    `dynamodbav:"eventDesc"`
	EventChecksum string    `dynamodbav:"eventChecksum"`
}

// ChangeReport represents the changes detected in the calendar
type ChangeReport struct {
	DeletedCount  int     `json:"deletedCount"`
	DeletedEvents []Event `json:"deletedEvents"`
	AddedCount    int     `json:"addedCount"`
	AddedEvents   []Event `json:"addedEvents"`
}

// Response represents the Lambda function response
type Response struct {
	StatusCode int               `json:"statusCode"`
	Body       string            `json:"body"`
	Headers    map[string]string `json:"headers"`
}

// generateEventKey creates a unique key for an event based on its date and description
func generateEventKey(date time.Time, checksum string) string {
	return fmt.Sprintf("%s_%s", date.Format("20060102"), checksum[:8])
}

// generateChecksum creates a SHA-256 checksum of the event description
func generateChecksum(description string) string {
	hash := sha256.Sum256([]byte(description))
	return fmt.Sprintf("%x", hash)
}

// processEvents compares current events with stored events and tracks changes
func processEvents(ctx context.Context, client *dynamodb.Client, events []Event) (*ChangeReport, error) {
	report := &ChangeReport{
		DeletedEvents: make([]Event, 0),
		AddedEvents:   make([]Event, 0),
	}

	// Get all existing events from DynamoDB
	existingEvents, err := getAllEvents(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("error getting existing events: %v", err)
	}

	// Create maps for easier comparison
	existingMap := make(map[string]DynamoDBEvent)
	for _, e := range existingEvents {
		existingMap[e.EventKey] = e
	}

	// Process current events
	currentMap := make(map[string]bool)
	for _, event := range events {
		checksum := generateChecksum(event.EventDescription)
		eventKey := generateEventKey(event.EventDate, checksum)
		currentMap[eventKey] = true

		// Check if this is a new event
		if _, exists := existingMap[eventKey]; !exists {
			report.AddedEvents = append(report.AddedEvents, event)

			// Store new event in DynamoDB
			dbEvent := DynamoDBEvent{
				EventKey:      eventKey,
				EventDate:     event.EventDate,
				EventDesc:     event.EventDescription,
				EventChecksum: checksum,
			}
			if err := putEvent(ctx, client, dbEvent); err != nil {
				return nil, fmt.Errorf("error storing new event: %v", err)
			}
		}
	}

	// Find deleted events
	for _, existingEvent := range existingEvents {
		if _, exists := currentMap[existingEvent.EventKey]; !exists {
			report.DeletedEvents = append(report.DeletedEvents, Event{
				EventDate:        existingEvent.EventDate,
				EventDescription: existingEvent.EventDesc,
			})

			// Delete event from DynamoDB
			if err := deleteEvent(ctx, client, existingEvent.EventKey); err != nil {
				return nil, fmt.Errorf("error deleting event: %v", err)
			}
		}
	}

	report.DeletedCount = len(report.DeletedEvents)
	report.AddedCount = len(report.AddedEvents)

	return report, nil
}

// getAllEvents retrieves all events from DynamoDB
func getAllEvents(ctx context.Context, client *dynamodb.Client) ([]DynamoDBEvent, error) {
	var events []DynamoDBEvent

	input := &dynamodb.ScanInput{
		TableName: aws.String(tableName),
	}

	result, err := client.Scan(ctx, input)
	if err != nil {
		return nil, err
	}

	err = attributevalue.UnmarshalListOfMaps(result.Items, &events)
	if err != nil {
		return nil, err
	}

	return events, nil
}

// putEvent stores a single event in DynamoDB
func putEvent(ctx context.Context, client *dynamodb.Client, event DynamoDBEvent) error {
	item, err := attributevalue.MarshalMap(event)
	if err != nil {
		return err
	}

	input := &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      item,
	}

	_, err = client.PutItem(ctx, input)
	return err
}

// deleteEvent removes a single event from DynamoDB
func deleteEvent(ctx context.Context, client *dynamodb.Client, eventKey string) error {
	input := &dynamodb.DeleteItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"eventKey": &types.AttributeValueMemberS{Value: eventKey},
		},
	}

	_, err := client.DeleteItem(ctx, input)
	return err
}

// HandleRequest is the Lambda handler function
func HandleRequest(ctx context.Context) (Response, error) {
	// Load AWS configuration
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return createErrorResponse(fmt.Errorf("unable to load SDK config: %v", err))
	}

	// Create DynamoDB client
	client := dynamodb.NewFromConfig(cfg)

	// Create HTTP client and fetch calendar data
	httpClient := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error creating request: %v", err))
	}

	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error making request: %v", err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error reading response body: %v", err))
	}

	// Extract events from HTML
	events, err := extractEvents(body)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error extracting events: %v", err))
	}

	// Process events and track changes
	report, err := processEvents(ctx, client, events)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error processing events: %v", err))
	}

	// Before creating the MailerSend request, fetch the API token
	cfg, err = config.LoadDefaultConfig(ctx,
		config.WithRegion("eu-central-1"), // Explicitly set the region
	)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error loading AWS config: %v", err))
	}
	secretsClient := secretsmanager.NewFromConfig(cfg)

	// Get the API token from AWS Secrets Manager
	secretResult, err := secretsClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String("prod/mailersend/apitoken"),
	})
	if err != nil {
		return createErrorResponse(fmt.Errorf("error getting secret: %v", err))
	}

	// Parse the secret JSON to extract the token
	rawSecret := *secretResult.SecretString
	var token string

	// Try parsing outer JSON first
	var secretData map[string]string
	if err := json.Unmarshal([]byte(rawSecret), &secretData); err != nil {
		// If JSON parsing fails, use the raw string as the token
		token = rawSecret
	} else {
		// Try all possible key formats (including the typo'd version)
		innerJSON := secretData["prod/mailersend/apitoken"]
		if innerJSON == "" {
			innerJSON = secretData["prod/mailerend/apitoken"] // Try the typo'd version
		}
		if innerJSON == "" {
			innerJSON = secretData["apitoken"]
		}

		if innerJSON != "" {
			// Try parsing the inner JSON value
			var innerData map[string]string
			if err := json.Unmarshal([]byte(innerJSON), &innerData); err == nil {
				// Try to get the token from the inner JSON
				token = innerData["prod/mailersend/apitoken"]
				if token == "" {
					token = innerData["prod/mailerend/apitoken"]
				}
				if token == "" {
					token = innerData["apitoken"]
				}
			}

			// If we couldn't parse the inner JSON or find a token, use the inner JSON as the token
			if token == "" {
				token = innerJSON
			}
		} else {
			// If we couldn't find any known keys, use the raw secret
			token = rawSecret
		}
	}

	if token == "" {
		return createErrorResponse(fmt.Errorf("token not found in secret"))
	}

	// Send email via MailerSend API
	type MailerSendRecipient struct {
		Email string `json:"email"`
	}

	type MailerSendFrom struct {
		Email string `json:"email"`
	}

	type MailerSendPayload struct {
		From    MailerSendFrom        `json:"from"`
		To      []MailerSendRecipient `json:"to"`
		Subject string                `json:"subject"`
		Text    string                `json:"text"`
		HTML    string                `json:"html"`
	}

	reportJSON, err := json.Marshal(report)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error marshaling report: %v", err))
	}

	payload := MailerSendPayload{
		From: MailerSendFrom{
			Email: "stefan.siebel@trial-3yxj6ljk8n5gdo2r.mlsender.net",
		},
		To: []MailerSendRecipient{
			{
				Email: "siebel.stefan@gmail.com",
			},
		},
		Subject: "ALS Calendar Update",
		Text:    string(reportJSON),
		HTML:    string(reportJSON),
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error marshaling email payload: %v", err))
	}

	mailerReq, err := http.NewRequest("POST", "https://api.mailersend.com/v1/email", bytes.NewBuffer(payloadBytes))
	if err != nil {
		return createErrorResponse(fmt.Errorf("error creating email request: %v", err))
	}

	mailerReq.Header.Set("Content-Type", "application/json")
	mailerReq.Header.Set("X-Requested-With", "XMLHttpRequest")
	mailerReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	mailerResp, err := httpClient.Do(mailerReq)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error sending email: %v", err))
	}
	defer mailerResp.Body.Close()

	if mailerResp.StatusCode != http.StatusAccepted && mailerResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(mailerResp.Body)
		return createErrorResponse(fmt.Errorf("email API error: status %d, response: %s", mailerResp.StatusCode, string(body)))
	}

	// Return successful response with calendar data
	return Response{
		StatusCode: 200,
		Body:       string(reportJSON),
		Headers: map[string]string{
			"Content-Type":                "application/json",
			"Access-Control-Allow-Origin": "*",
		},
	}, nil
}

// createErrorResponse creates an error response
func createErrorResponse(err error) (Response, error) {
	return Response{
		StatusCode: 500,
		Body:       fmt.Sprintf(`{"error":"%s"}`, err.Error()),
		Headers: map[string]string{
			"Content-Type":                "application/json",
			"Access-Control-Allow-Origin": "*",
		},
	}, nil
}

// extractEvents finds all tags with class="events" and extracts their dates and descriptions.
func extractEvents(body []byte) ([]Event, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("error parsing HTML: %v", err)
	}

	var events []Event

	var traverse func(*html.Node)
	traverse = func(n *html.Node) {
		if n.Type == html.ElementNode {
			// Check if the node has class="events"
			var hasEventsClass bool
			var ariaLabel string
			var description string

			for _, attr := range n.Attr {
				if attr.Key == "class" && attr.Val == "events" {
					hasEventsClass = true
					// Get the description from the node's text content
					description = cleanText(getTextContent(n))
				}
				if attr.Key == "aria-labelledby" {
					ariaLabel = attr.Val
				}
			}

			// If we found a tag with class="events" and it has an aria-labelledby attribute
			if hasEventsClass && ariaLabel != "" {
				// Split by dash and take the right part
				parts := strings.Split(ariaLabel, "-")
				if len(parts) > 1 {
					dateStr := parts[len(parts)-1]
					// Parse the date string (YYYYMMDD)
					if len(dateStr) == 8 {
						date, err := time.Parse("20060102", dateStr)
						if err == nil {
							event := Event{
								EventDate:        date,
								EventDescription: description,
							}
							events = append(events, event)
						}
					}
				}
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			traverse(c)
		}
	}

	traverse(doc)
	return events, nil
}

// getTextContent extracts all text content from a node and its children
func getTextContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var result string
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		result += getTextContent(c)
	}
	return result
}

// cleanText removes extra whitespace and formats the text properly
func cleanText(s string) string {
	// Replace multiple spaces, newlines and tabs with a single space
	re := regexp.MustCompile(`[\s\p{Zs}]+`)
	s = re.ReplaceAllString(s, " ")

	// Remove any remaining whitespace at the start or end
	s = strings.TrimSpace(s)

	// Replace " – " with " - " for consistency
	s = strings.ReplaceAll(s, " – ", " - ")

	return s
}

func main() {
	lambda.Start(HandleRequest)
}
