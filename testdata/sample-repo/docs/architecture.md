# Architecture Overview

This document describes the high-level architecture of the sample application.

## Components

### Authentication Layer

The authentication layer is implemented in `src/auth.ts` and handles:
- JWT token generation and verification
- Middleware for protected routes
- Role-based access control

The `AuthMiddleware` class is the primary entry point for route protection.

### Data Layer

All database interactions go through repository classes:
- `UserRepository` — user CRUD and password verification
- `ProductController` — product catalogue management
- `OrderController` — order lifecycle

### API Layer

The Express application in `src/api.ts` wires together all components.
Public routes handle authentication (login, register).
Protected routes require a valid Bearer token.

## Security Decisions

- Passwords hashed with bcrypt (12 salt rounds)
- JWT tokens expire according to the `JWT_EXPIRES_IN` environment variable
- Role checking happens in the middleware layer, not in controllers

## Data Flow

```
Client → Express Router → AuthMiddleware → Controller → Repository → Database
```
