# Flight Booking System - Temporal Architecture

Source: `DOCS/Flight Booking System.pdf` (see the PDF for the architecture diagram).

The flight booking flow goes from User through Web App, RESTful Server, Temporal, and finally to Workers.

## User Flow Exercise

**Scenario:** Flight seat reservation and payment system with timeouts and validation.

## Architecture (from the diagram)

| Layer | Component | Responsibilities |
|---|---|---|
| User Layer | User | Creates flight order, selects N seats, reviews and pays |
| User Layer | Web App | Real-time status, seat timer display, order updates |
| Application Layer | RESTful Server (Go) | Flight APIs, payment gateway, order management |
| Temporal Platform | Temporal Server (`temporal server start-dev`) | Workflow orchestration, timeout management |
| Temporal Platform | Temporal Workers (Go) | Seat reservation, payment processing, timer management |
| Business Logic | Seat Reservation | Seat inventory management, 15 min timeout, refreshable timer |
| Business Logic | Payment Validation | 5-digit code check, 10 sec timeout, 3 retry attempts, 15% failure rate |
| Business Logic | Order Management | Status tracking, failure handling, confirmation |

Request flow: User -> Web App -> RESTful Server -> Temporal Server -> Workers -> Business Logic. Solid arrows in the diagram are the request flow; dotted arrows are the response/result flow back to the user.

## Architecture Components

- **User:** Customer booking flights, selecting seats, and making payments.
- **Web App:** Frontend showing real-time seat timer, order status, and booking interface.
- **RESTful Server (Go):** Go-based APIs for flight management, payment gateway integration, and order processing.
- **Temporal Server:** Orchestrates complex booking workflows with timeouts and state management.
- **Workers (Go):** Execute specific booking activities and handle business logic.

## Business Logic

- **Seat Reservation:** Manages seat inventory and 15-minute seat holds with refreshable timers when the user updates the selection.
- **Payment Validation:** Processes 5-digit payment codes with a 10-second timeout, 3 retry attempts, 15% failure rate simulation.
- **Order Management:** Tracks order status, handles failures, sends confirmations.

## Mandatory Temporal Workflows

- At the very least, there must be a Temporal workflow that implements the entire order.

## Detailed User Flow

1. **Create Flight Order:** User initiates the booking process.
2. **Select N Seats:** User chooses seats -> **Seat Reservation Workflow starts** (15 min timer).
3. **Review Order:** User can see the timer countdown and modify seats (timer refreshes on changes).
4. **Pay with 5-digit Code:** User enters payment code -> **Payment Code Validation**.
5. **Code Validation:** System checks the code within 10 seconds.
   - **Success (85%):** Charge user -> **Order Management** -> confirmation message.
   - **Failure (15%):** Retry up to 3 times -> after 3 failures, the order fails with an indicative message.
6. **Real-time Updates:** User always sees the seat timer and order status.

## Business Rules Implemented by Temporal

- Seat inventory management (implementation approach left to the developer)
- 15-minute seat reservation with auto-release
- Timer refresh on seat updates
- 10-second payment validation timeout
- 3 retry attempts for failed payments
- 15% payment failure simulation
- Real-time status tracking
- Graceful failure handling with user feedback

## Implementation Notes

**Seat Management:** The system must handle seat inventory internally. It is not delegated to external microservices. The implementation approach is flexible and could include:

- Custom data structures with conflict resolution
- In-memory state with persistence
- Temporal Entity Workflows for seat state management
- Traditional database with transaction management

Choice of approach is left to the implementer.
