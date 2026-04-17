# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build and deploy manually
./build.sh

# Build only (Linux binary for Lambda)
GOOS=linux GOARCH=amd64 go build -o bootstrap main.go

# Run tests
go test ./...

# Download dependencies
go mod download
```

CI/CD is handled by AWS CodeBuild using `buildspec.yml`, which builds the binary, packages it as `function.zip`, and deploys to the `ALSCalendarParser` Lambda via CodeDeploy.

## Architecture

Single-file Go AWS Lambda (`main.go`) that:

1. **Fetches** the ALS Usingen WordPress calendar by POSTing to the `admin-ajax.php` endpoint with a hardcoded nonce/args (`calendarPostBody`). These values were captured from browser traffic and may need updating if the site changes.

2. **Parses** the HTML response — looks for `<* class="events" aria-labelledby="*-YYYYMMDD">` nodes and extracts date + text content.

3. **Diffs** against events stored in DynamoDB (`ALSEvents` table, `eu-central-1`). Events are keyed by `date_checksum[:8]`. New events are inserted; events no longer on the calendar are deleted.

4. **Emails** a summary via AWS SES SMTP (`email-smtp.eu-central-1.amazonaws.com`) to a Google Group. SMTP credentials are fetched from AWS Secrets Manager (`prod/eu-central-1/smtp`). Email is sent when: new events were added OR it's Friday (weekly digest).

## Key details

- The `calendarPostBody` constant contains a WordPress nonce (`r34ics_nonce`) and `args` hash that may expire or change when the site updates — this is the most likely breakage point.
- DynamoDB table name: `ALSEvents`, partition key: `eventKey` (string).
- Secrets Manager secret key names: `ses-smtp-username-eu-central-1` and `ses-smtp-password-eu-central-1`.
- Email recipient: `als-kalender-updates@googlegroups.com`, sender: `stefan@stefansiebel.de`.
- Upcoming events window: 60 days from now.
