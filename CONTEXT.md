# BugHan — error-tracking context

BugHan ingests error and performance telemetry from Sentry-compatible SDKs, groups related events into issues, and helps a small team triage them. This file is the canonical glossary for DESIGN.md, code, and UI. Resolved in ticket [#21](https://github.com/Atash03/BugHan/issues/21).

## Language

### Tenancy

**Organization**:
The tenant — a bounded workspace owning projects, members, and settings. Every piece of data belongs to exactly one organization.
_Avoid_: workspace, account, tenant

**User**:
A person who logs into BugHan (email + password). Exists globally; acts inside an organization only through a Membership.
_Avoid_: account, profile

**Membership**:
The join between a User and an Organization, carrying that user's single role there (`owner`, `admin`, `member`).
_Avoid_: team membership, participation

**Invitation**:
A pending, expiring, tokenized offer (delivered as an emailed link) for one person to become a Member of an organization at a chosen role.
_Avoid_: invite link (as an entity)

### Projects & credentials

**Project**:
The unit of ingestion and reporting: owns its DSN(s), issues, environments, releases, and settings. Belongs to exactly one organization.
_Avoid_: app, client

**Ingest Key**:
A project-scoped public key (UUID) embedded in the DSN that authenticates SDK traffic to that project. Several may exist per project for rotation; revoking one stops accepting its traffic.
_Avoid_: client key, secret key (deprecated in the protocol)

**DSN**:
The full connection string handed to an SDK — scheme, host, path, Ingest Key, project id. Derived from Project + Ingest Key; not an independent stored object.
_Avoid_: API key, token

**API Token**:
A secret, User-owned credential for BugHan's own REST API (scripts, CI source-map upload). Its power derives from the owner's Memberships.
_Avoid_: ingest key, DSN key

### Telemetry

**Event**:
One immutable telemetry payload accepted at ingest — an error event, a transaction, or a session — belonging to one Project.
_Avoid_: log, crash (informal)

**Issue**:
A durable grouping of related Events within one Project (via grouping fingerprint), carrying status, assignment, counts, and first/last-seen. The unit a human triages.
_Avoid_: group, bug (Sentry's internal "Group" maps to Issue)

**Environment**:
A named deployment context of a Project (e.g. `production`, `staging`) that events declare; a first-class, filterable dimension.
_Avoid_: stage, channel

**Release**:
A version string of the deployed app within a Project that events declare; anchors release health and source-map association. First-class entity.
_Avoid_: build, deploy

**Tag**:
A string→string attribute (≤200 chars) attached to an Event and summarized onto its Issue. Not an entity of its own.
_Avoid_: label, custom field

**Affected User**:
An unverified identity string an event carries about the monitored app's end-user (id/email/ip). Never a login-capable User.
_Avoid_: bare "user" — that always means a BugHan User here

## Roles (v0.1)

Org-wide roles on a Membership — no per-project ACLs:

- **owner** — everything, plus destroying/transferring the organization
- **admin** — manage projects, members, invitations, Ingest Keys, project settings; creates projects
- **member** — read all projects; triage: assign/resolve/ignore issues, comment

## Relationships

- An **Organization** has many Memberships; a **User** holds many Memberships (one role each).
- An **Organization** owns many **Projects**; a Project never moves between organizations (v0.1).
- A **Project** exposes one or more **Ingest Keys**; each DSN = host + project id + one Ingest Key.
- An **Event** arrives for exactly one **Project**, may declare one **Environment** and one **Release**, carries Tags, and — if an error — joins exactly one **Issue**.
- An **Issue** belongs to one **Project** with an optional assignee (**User**, via Membership) and Comments authored by Members.
- **API Tokens** belong to a **User**; their reach is the union of that User's Memberships.

## Example dialogue

> **Dev:** "The SDK posts with a valid DSN but gets a 403 — is the project broken?"
> **Domain:** "No — the DSN embeds an Ingest Key. Revoked key, dead traffic; the Project is untouched. Rotate: mint a new key, redeploy, revoke the old."

> **Dev:** "Can an affected user click a link to view their crash?"
> **Domain:** "No — an Affected User isn't a User. Only Members authenticate; affected-user strings are labels on Events."

## Flagged ambiguities

- **Teams**: the superseded local map draft said orgs → teams → members; the live map and ticket #21 resolve v0.1 as flat Organization → Project. A future Teams layer attaches at the permission seam (grants reference org+project), never at issues/events. "Team" is deliberately absent from the language until that effort.
- **"user"** meant both a login-capable person and the identity string on events — resolved: **User** vs **Affected User**.
- **"key"** meant both the DSN's public key and an API credential — resolved: **Ingest Key** vs **API Token**.

## Deliberately out (v0.1, with owners)

Per-project access control · read-only/viewer role · org join requests · per-user notification preferences · SSO/SAML · 2FA (confirmed out in #22) · cross-org project moves.
