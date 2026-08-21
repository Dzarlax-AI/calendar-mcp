# Privacy Policy

**Effective date: August 21, 2026**

Calendar Platform is an open-source calendar connection, synchronization, and MCP service. This policy explains how the hosted Calendar Platform instance at `calendar.dzarlax.dev` (the **Service**) handles information. Independently self-hosted installations are controlled by their respective operators, who are responsible for their own privacy practices.

## Information the Service accesses

When you connect a calendar provider, the Service may access:

- account and calendar metadata, including account display names, calendar names and identifiers, time zones, and read/write capabilities;
- OAuth access and refresh tokens for Google Calendar and Microsoft 365, or an Apple identifier and app-specific password for Apple Calendar;
- calendar event data needed for requested operations or configured synchronization, including titles, descriptions, locations, start and end times, recurrence information, status, visibility, and availability;
- configuration and operational data, including sync rules, synchronization windows, provider event identifiers, content hashes, run times, outcome counters, and sanitized error categories; and
- technical request data normally produced by the web server and reverse proxy, such as timestamps, network addresses, and user-agent information.

The Service does not copy event attendees, create invitations, or request provider notifications as part of synchronization.

## How information is used

The Service uses this information only to:

- authenticate the connected provider account;
- discover calendars and their capabilities;
- perform calendar actions explicitly requested through the web interface, MCP, or API;
- copy selected event fields from a source calendar to a target calendar under a configured one-way sync rule;
- preserve supported recurring events and reconcile previously copied events; and
- secure, operate, diagnose, and maintain the Service.

Use and transfer of information received from Google APIs adheres to the [Google API Services User Data Policy](https://developers.google.com/terms/api-services-user-data-policy), including its Limited Use requirements.

## Storage and security

Provider credentials are encrypted at the application layer before they are stored. The encryption key is kept separately from the database. OAuth authorization attempts use expiring, single-use state and PKCE protections.

The Service does not persist a separate full copy of source calendar event bodies. It stores the provider identifiers, synchronization mappings, content hashes, rules, and sanitized run information required to operate and reconcile synchronization. Event content is processed in memory and may be written to a target calendar only when a configured rule or explicit calendar operation requires it.

No method of storage or transmission is completely secure. The operator applies reasonable technical controls but cannot guarantee absolute security.

## Sharing and disclosure

The Service does not sell personal information and does not use calendar data for advertising.

Information is disclosed only as needed to provide the Service:

- to Google, Microsoft, or Apple when reading from or writing to a connected provider;
- from a selected source provider to a selected target provider when a sync rule is executed;
- to infrastructure providers that host or transmit the Service's application and database data; or
- when required by law, necessary to protect the Service or its users, or requested by the person who connected the account.

Each calendar provider processes information under its own privacy policy and account terms.

## Retention and deletion

Connection credentials and discovered calendar metadata are retained while a provider connection remains configured. Sync rules, event mappings, and run metadata are retained while needed to operate and diagnose configured synchronization.

You can stop future provider access by revoking Calendar Platform in the provider's account settings. Removing a connection from Calendar Platform deletes its stored credentials and discovered calendar records once rules that reference the connection have been removed. Revoking or deleting a connection does not automatically delete events that were already copied into a target calendar.

For deletion or privacy requests concerning the hosted instance, open a minimal request through the [project issue tracker](https://github.com/Dzarlax-AI/calendar-mcp/issues). Do not include credentials, calendar content, or other sensitive information in a public issue; the operator will arrange a private channel if needed.

## Your choices

You decide which provider accounts and calendars to connect, which one-way sync rules to configure, and the past/future synchronization window for each rule. You may revoke provider authorization at any time. Provider account settings may offer additional access, export, and deletion controls.

## Changes to this policy

This policy may be updated when the Service, its data handling, or legal requirements change. The effective date above will be updated when material changes are published.

## Contact

The Service is operated as **Calendar Platform**. For privacy questions, use the [project issue tracker](https://github.com/Dzarlax-AI/calendar-mcp/issues) without posting sensitive information.
