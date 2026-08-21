# Terms of Service

**Effective date: August 21, 2026**

These Terms govern use of the hosted Calendar platform instance at `calendar.dzarlax.dev` (the **Service**). The Calendar platform source code can also be deployed independently; a self-hosted installation is operated under the responsibility and terms of its deployer.

## The Service

Calendar platform connects Google Calendar, Microsoft 365, and Apple Calendar accounts, exposes calendar operations to MCP and API clients, and runs configured one-way synchronization rules between calendars.

The hosted Service is a private, single-user control plane. Public access to the landing page and policy documents does not grant access to the application or its connected calendar accounts.

## Authorization and acceptable use

You may connect and operate only accounts and calendars that you are authorized to access. By connecting a provider, you authorize the Service to use the permissions displayed by that provider for the functions described in the [Privacy Policy](/privacy).

You agree not to use the Service to violate applicable law, third-party rights, provider terms, or technical access controls; interfere with the Service; distribute malicious content; or attempt unauthorized access to calendars, credentials, or infrastructure.

## Calendar changes and synchronization

Calendar operations can create, update, move, or delete events. A sync rule may reproduce later source changes in its target calendar. You are responsible for selecting the correct source, target, synchronization window, and provider accounts, and for reviewing the dry-run result before enabling a rule.

The Service is designed to reduce accidental side effects: rules are one-way, start paused, reject cycles, require a dry run, do not copy attendees, and request no invitations or provider notifications. These controls do not eliminate every risk. Provider limitations, outages, concurrent edits, configuration errors, or unsupported recurrence can delay, reject, or produce unexpected calendar changes.

Maintain appropriate backups or recovery options for important calendars. Test new rules with dedicated calendars before using them with production data.

## Third-party services

Google, Microsoft, Apple, hosting providers, MCP clients, and other connected systems are independent third parties. Their terms, privacy policies, availability, and technical limitations apply separately. Calendar platform does not control and is not responsible for those services.

## Availability and changes

The Service may be changed, suspended, limited, or discontinued at any time, including for maintenance, security, provider changes, or misuse. Features and provider compatibility may change as external APIs and protocols evolve.

## Disclaimer and limitation of liability

To the maximum extent permitted by applicable law, the Service is provided **as is** and **as available**, without warranties of uninterrupted operation, fitness for a particular purpose, non-infringement, or preservation of calendar data.

To the maximum extent permitted by applicable law, the operator is not liable for indirect, incidental, special, consequential, or exemplary damages, or for loss of data, calendar events, access, business, or profits arising from use of or inability to use the Service.

Some jurisdictions do not allow particular warranty exclusions or liability limitations, so parts of this section may not apply to you.

## Termination

You may stop using the Service and revoke its provider access at any time. The operator may suspend or terminate access when necessary to protect accounts or infrastructure, comply with law, respond to provider requirements, or enforce these Terms.

Revoking access or removing a connection does not automatically remove events already copied to another calendar.

## Changes to these Terms

These Terms may be updated when the Service or applicable requirements change. The effective date above will be updated when revised Terms are published. Continued use after an update constitutes acceptance of the revised Terms to the extent permitted by law.

## Contact

For questions about these Terms, use the [project issue tracker](https://github.com/Dzarlax-AI/calendar-mcp/issues). Do not post credentials, calendar content, or other sensitive information in a public issue.
