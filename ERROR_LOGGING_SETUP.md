# Error Logging Setup Test

This file demonstrates how to configure error logging to a Telegram group.

## Configuration Setup

Add the following to your `config.yaml`:

```yaml
telegram:
  message_dispose_duration: 10
  api_key: "YOUR_BOT_TOKEN"
  log_group_id: -1001234567890    # Your Telegram group chat ID
  error_thread_id: 0              # Optional: specific thread ID (0 for main chat)
```

## How to Get Group ID

1. Add your bot to the group
2. Send a message in the group
3. Visit: `https://api.telegram.org/bot<YOUR_BOT_TOKEN>/getUpdates`
4. Look for the chat object and copy the negative ID

## Features

The error logger will automatically log:

### Payment Failures
- User information (who attempted the payment)
- Payment amount and memo
- Invoice details (truncated for security)
- Detailed failure reason

### Transaction Failures
- Sender and receiver information
- Transaction type (send/tip/etc.)
- Amount being transferred
- Specific error message

### Database Errors
- Operation being performed
- User information
- Database error details

### Critical Errors & Panics
- Full stack trace
- Context information
- Immediate notification for critical issues

## Example Error Messages

```
❌ **ERROR LOG**

**Time:** 2025-06-26 12:34:56 UTC
**Context:** Payment Failure - Amount: 1000 sat, Memo: Coffee payment
**Error:** `insufficient funds`

**Details:** User: @username (ID: 123456789), Payment Details:
- From: @username (ID: 123456789)
- Amount: 1000 sat
- Memo: Coffee payment
- Failure Reason: insufficient funds

**Location:** `pay.go:212` in `confirmPayHandler`
```

## Environment Variables (Alternative)

You can also set these via environment variables:
- `TELEGRAM_LOG_GROUP_ID`
- `TELEGRAM_ERROR_THREAD_ID`

## Disable Error Logging

Set `log_group_id: 0` to disable error logging to Telegram.
