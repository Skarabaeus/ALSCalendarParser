package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"regexp"
	"sort"
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
	url           = "https://als-usingen.de/kalender/"
	userAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15"
	tableName     = "ALSEvents"
	emailTemplate = `<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>ALS Kalender Update</title>
</head>
<body style="font-family: Arial, sans-serif; margin: 20px; padding: 20px; background-color: #f9f9f9;">
    <h1 style="text-align: center; color: #333;">ALS Kalender Update</h1>
    <table align="center" width="100%" style="max-width: 600px; background-color: #ffffff; padding: 20px; border-radius: 5px; box-shadow: 0 0 10px rgba(0,0,0,0.1);">
        <tr>
            <td>
                {list_placeholder}
            </td>
        </tr>
    </table>
</body>
</html>`
	listTemplate = `
<h2 style="text-align: center; color: #333;">{title_list}</h2>

<ul style="color: #666;">
    {list_items}
</ul>
`
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
	DeletedCount   int     `json:"deletedCount"`
	DeletedEvents  []Event `json:"deletedEvents"`
	AddedCount     int     `json:"addedCount"`
	AddedEvents    []Event `json:"addedEvents"`
	UpcomingEvents []Event `json:"upcomingEvents"`
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
		DeletedEvents:  make([]Event, 0),
		AddedEvents:    make([]Event, 0),
		UpcomingEvents: make([]Event, 0),
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

	// Get the date range for upcoming events
	now := time.Now()
	cutoff := now.AddDate(0, 0, 60) // 60 days from now

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

		// Check if this is an upcoming event (within next 60 days)
		if event.EventDate.After(now) && event.EventDate.Before(cutoff) {
			report.UpcomingEvents = append(report.UpcomingEvents, event)
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

	// Sort upcoming events by date
	sort.Slice(report.UpcomingEvents, func(i, j int) bool {
		return report.UpcomingEvents[i].EventDate.Before(report.UpcomingEvents[j].EventDate)
	})

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

	// Marshal the report to JSON
	reportJSON, err := json.Marshal(report)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error marshaling report: %v", err))
	}

	if report.AddedCount > 0 || time.Now().Weekday() == time.Friday {
		emailBody, err := createBody(report)
		if err != nil {
			return createErrorResponse(fmt.Errorf("error creating email body: %v", err))
		}

		err = sendEmail(emailBody)
		if err != nil {
			return createErrorResponse(fmt.Errorf("error sending email: %v", err))
		}
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

func createBody(report *ChangeReport) (string, error) {
	// Create the first list for changed events
	changedEventsList := ""
	for _, event := range report.AddedEvents {
		changedEventsList += fmt.Sprintf("<li><b>%s</b><br />%s<br /><br /></li>",
			event.EventDate.Format("02.01.2006"),
			event.EventDescription)
	}
	changedEventsSection := ""
	if report.AddedCount > 0 {
		changedEventsSection = strings.ReplaceAll(listTemplate, "{title_list}", "Geänderte Kalendereinträge")
		changedEventsSection = strings.ReplaceAll(changedEventsSection, "{list_items}", changedEventsList)
	}

	// Create the second list for upcoming events
	upcomingEventsList := ""
	for _, event := range report.UpcomingEvents {
		upcomingEventsList += fmt.Sprintf("<li><b>%s</b><br />%s<br /><br /></li>",
			event.EventDate.Format("02.01.2006"),
			event.EventDescription)
	}
	upcomingEventsSection := strings.ReplaceAll(listTemplate, "{title_list}", "Einträge für die nächste 60 Tage")
	upcomingEventsSection = strings.ReplaceAll(upcomingEventsSection, "{list_items}", upcomingEventsList)

	combinedLists := upcomingEventsSection
	if changedEventsList != "" {
		combinedLists = changedEventsSection + upcomingEventsSection
	}

	// Replace the placeholder in the email template
	finalEmail := strings.ReplaceAll(emailTemplate, "{list_placeholder}", combinedLists)

	return finalEmail, nil
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

func sendEmail(body string) error {
	// SMTP server configuration
	smtpHost := "email-smtp.eu-central-1.amazonaws.com"
	smtpPort := "587"

	username, password := getSmtpCredentials()

	// Sender and recipient
	from := "stefan@stefansiebel.de"
	to := []string{"als-kalender-updates@googlegroups.com"}

	// Email headers
	headers := make(map[string]string)
	headers["From"] = from
	headers["To"] = strings.Join(to, ",")
	headers["Subject"] = fmt.Sprintf("ALS Kalender Update - %s", time.Now().Format("02.01.2006"))
	headers["MIME-Version"] = "1.0"
	headers["Content-Type"] = "text/html; charset=UTF-8"

	// Build message with headers
	message := ""
	for key, value := range headers {
		message += fmt.Sprintf("%s: %s\r\n", key, value)
	}
	message += "\r\n" + body

	// Authentication
	auth := smtp.PlainAuth("", username, password, smtpHost)

	// Send the email
	err := smtp.SendMail(smtpHost+":"+smtpPort, auth, from, to, []byte(message))
	if err != nil {
		return err
	}

	return nil
}

func getSmtpCredentials() (string, string) {
	secretName := "prod/eu-central-1/smtp"
	region := "eu-central-1"

	config, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		log.Fatal(err)
	}

	// Create Secrets Manager client
	svc := secretsmanager.NewFromConfig(config)

	input := &secretsmanager.GetSecretValueInput{
		SecretId:     aws.String(secretName),
		VersionStage: aws.String("AWSCURRENT"), // VersionStage defaults to AWSCURRENT if unspecified
	}

	result, err := svc.GetSecretValue(context.TODO(), input)
	if err != nil {
		log.Fatal(err.Error())
	}

	// Decrypts secret using the associated KMS key.
	var secretString string = *result.SecretString

	// Parse the JSON to get both secrets
	var secretData map[string]string
	if err := json.Unmarshal([]byte(secretString), &secretData); err != nil {
		log.Fatal(err.Error())
	}

	// Extract username and password
	username := secretData["ses-smtp-username-eu-central-1"]
	password := secretData["ses-smtp-password-eu-central-1"]

	return username, password

}
func main() {
	lambda.Start(HandleRequest)
}
