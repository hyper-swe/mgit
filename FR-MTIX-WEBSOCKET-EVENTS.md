# Feature Request: WebSocket Event Subscription for mgit Integration

**From:** mgit development team
**To:** mtix maintainer team
**Date:** 2026-04-07
**Priority:** Medium

## Summary

mgit requires real-time event subscription from mtix to trigger auto-squash workflows when tasks are marked complete. The current mtix WebSocket endpoint (`/ws/events`) supports this, but mgit's approved dependency list does not include a WebSocket client library.

## Request

Evaluate one of the following options:

### Option A: Server-Sent Events (SSE) Endpoint
Add an SSE endpoint (e.g., `GET /api/v1/events/stream`) as an alternative to WebSocket. SSE uses standard HTTP and requires no additional client libraries — Go's `net/http` stdlib handles it natively.

### Option B: HTTP Long-Polling Endpoint
Add `GET /api/v1/events/poll?since=<timestamp>` returning events since the given timestamp. Simpler than WebSocket, no special client needed.

### Option C: Webhook Callback Registration
Add `POST /api/v1/webhooks` allowing mgit to register a callback URL. When a task status changes, mtix POSTs an event to the registered URL.

## Context

- mgit's `APPROVED-PACKAGES.md` does not include `gorilla/websocket` or any WebSocket library
- Adding a new dependency requires the full package approval process (security review, CGO-free verification, license check)
- mgit currently uses polling as a fallback (30s interval), which works but is not ideal for latency-sensitive workflows
- The auto-squash-on-task-completion feature (FR-14.3) benefits most from real-time events

## Impact

Without real-time events, mgit polls mtix every 30 seconds. This means up to 30s delay between task completion in mtix and auto-squash in mgit. For most workflows this is acceptable, but real-time would improve the developer experience.

## Acceptance Criteria

- mgit can subscribe to task status change events using only Go stdlib
- Events include: `node_id`, `status` (old/new), `timestamp`, `author`
- Filtering by project prefix supported (e.g., only MGIT-* events)
- Connection resilience: auto-reconnect on network failure
