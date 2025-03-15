package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"golang.org/x/net/html"
)

const (
	url       = "https://als-usingen.de/kalender/"
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15"
)

// Event represents a calendar event with a date and description
type Event struct {
	// EventDate stores the date of the event
	EventDate time.Time `json:"date"`
	// EventDescription stores the full description of the event, including time and details
	EventDescription string `json:"description"`
}

// Response represents the Lambda function response
type Response struct {
	StatusCode int               `json:"statusCode"`
	Body       string            `json:"body"`
	Headers    map[string]string `json:"headers"`
}

// HandleRequest is the Lambda handler function
func HandleRequest(ctx context.Context) (Response, error) {
	// Create a new HTTP client
	client := &http.Client{}

	// Create a new request
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error creating request: %v", err))
	}

	// Set User-Agent header
	req.Header.Set("User-Agent", userAgent)

	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error making request: %v", err))
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error reading response body: %v", err))
	}

	// Extract events
	events, err := extractEvents(body)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error extracting events: %v", err))
	}

	// Convert events to JSON
	jsonData, err := json.Marshal(events)
	if err != nil {
		return createErrorResponse(fmt.Errorf("error marshaling events to JSON: %v", err))
	}

	// Return successful response
	return Response{
		StatusCode: 200,
		Body:       string(jsonData),
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
