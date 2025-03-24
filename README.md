# ALSCalendarParser

##What does this script do?

On its website, the Astrid Lindgren Primary School in Usingen provides a calendar where all events are announced well in advance. Unfortunately, proactive email notifications about upcoming events or timetable updates are sometimes sent out very late.  

For parents, it’s best to learn about any schedule changes as early as possible. To address this, I implemented a script that parses the school's calendar and posts updates to a [Google Groups group](https://groups.google.com/g/als-kalender-updates) that I created for this purpose.  

Parents can sign up for the group and will receive an email whenever the school's website calendar is updated. If there are no schedule changes, no email will be sent. However, every Friday, the script sends an email summarizing the events for the next 60 days, even if there are no changes.  

## How does it work

Here are a few notes on the infrastructure that runs this script:
- The script executed as Lambda function on AWS. 
- AWS EventBridge triggers daily execution.
- Events are stored in DynamoDB. With each execution DynamoDB table data is compared to new data which is parsed from the web page. Deleted events will be deleted from the table, new events will be added. 
- Emails to Google Groups are send via AWS SES smtp.
- A AWS CodePipeline automates build and deployment after each git push


