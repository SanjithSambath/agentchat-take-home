# AgentMail Take-Home Project

Welcome to the AgentMail take-home project. The goal of this project is to assess your ability to make code, documentation, and infrastructure decisions. The use of AI is allowed and encouraged.

## The Problem

Design and implement a **stream-based agent-to-agent messaging service** — a platform where AI agents can communicate with each other, with durable message history.

Agents are powered by LLMs. They may be online at the same time and want to converse in real time, streaming tokens as they are generated. They may also be offline — in which case, messages sent to them should be stored durably so they can catch up when they reconnect. The service must handle both of these scenarios, in both one-on-one and group conversations.

The core messaging design — how messages are structured, ordered, delivered, and streamed — is up to you. You will need to make decisions about message ordering, delivery guarantees, streaming semantics, and more. We care as much about *which* decisions you make and *why* as we do about the implementation itself.

## Resources

You are provided with:

- An **Anthropic API key** for both development assistance and powering your agents.
- A **credit card** for any infrastructure you need to provision.

## Requirements

### Streaming

The service must support streaming in both directions — clients must be able to stream writes to the server (e.g., token by token) and stream reads from the server (receiving content in real time). These do not need to be on the same connection, and you may support any combination of transports:

- **WebSocket** — a single connection that streams in both directions. Widely supported, simple to set up, but requires defining your own message schema.
- **SSE** — server-to-client streaming over a long-lived HTTP response. Simple for read-side tailing.
- **HTTP GET/POST** — standard request-response for reading and writing.
- **gRPC streaming** — separate server-streaming and client-streaming RPCs (or a single bidirectional stream). Strongly typed via protobuf with client codegen.

You are free to mix these — for example, SSE for streaming reads, HTTP POST for simple writes, and WebSocket for bidirectional real-time sessions. One important consideration: the primary clients of this service are AI coding agents (e.g., Claude Code). Your choice of transport should account for how easily these agents can work with it.

### Service API

The following operations are **required**. Their behavior is defined here so you can focus your design effort on the core messaging problem. The API style (REST, gRPC, WebSocket, etc.) is your choice.

#### Agents

- **Register**: Provisions a new agent and returns a server-assigned opaque identifier (no metadata). Each call creates a new, distinct identity. There is no authentication — the agent's identifier is how the server scopes access.

#### Conversations

- **Create conversation**: Creates a new conversation with a server-assigned identifier. The creator is automatically a member. Conversations are not uniquely determined by their participants — the same set of agents can have multiple separate conversations.
- **List conversations**: Returns all conversations the agent is a member of. Each entry includes at minimum the conversation ID and the current member list.
- **Invite**: Adds another agent to a conversation by its identifier. The invitee is immediately added (no acceptance step). Inviting an agent that is already a member is a no-op. Inviting a nonexistent agent is an error. There are no roles or ownership — any member can invite any agent. When an agent is added, it has access to the full conversation history.
- **Leave**: Removes the calling agent from a conversation. A leave by the last remaining member is rejected — a conversation must have at least one member at all times. An agent that leaves can be re-invited; its prior messages remain attributed to it. Active streaming connections are terminated on leave.

A single agent can create a conversation with itself. There is no limit on the number of participants. Only members can read from or write to a conversation.

## Technical Guidance

These are recommendations, not rigid requirements. If you believe a different approach is a better fit, you may take it — but you must document your reasoning clearly.

- We recommend [S2](https://s2.dev) as your stream storage primitive. S2 provides durable, ordered streams with real-time tailing and replay. Refer to the [S2 documentation](https://s2.dev/docs) for details. All other infrastructure and technology choices are yours to make.
- We recommend that the server track each agent's read position within a conversation, so that agents can resume from where they left off after disconnecting.

## Scope

The focus of this project is the **core messaging service** — purely plaintext communication (no multimedia or rich formatting) between any number of agents. Do not implement authentication, authorization, user management, or any other peripheral concerns. Spend your time on the core problem.

## Design Questions

The requirements above cover the operational surface and streaming transport. The hard part is the messaging layer underneath. As you design and build, consider the following:

- How should messages be structured on the stream? What is the unit of communication?
- What ordering guarantees should the system provide? At what granularity?
- What delivery guarantees should the system provide, and to whom?
- When should a write be considered successful?
- How should multiple agents writing to the same conversation at the same time be handled?
- How should the service handle an agent that crashes or disconnects while composing a message?
- How should the service behave when an agent reconnects after being offline?

Your service must mediate all client access to the underlying storage. Clients should interact with your service's API — not read from or write to backing streams directly.

## Deliverables

1. **Codebase**: A complete, working implementation of the messaging service with tests.
2. **Documentation**: Markdown files covering:
   - Every design decision you made in your implementation and why.
   - How you would revisit those design decisions given no restrictions on time and resources. This is not about adding new features — it is about infrastructure, reliability, scale, and the overall design quality of the core feature set you chose to implement.
3. **Live service**: Your messaging service must be deployed and running on infrastructure you host, with at least one Claude-powered agent running on it that we can converse with. We will also evaluate the service by connecting our own agents as clients — both to converse with your agents and to converse with each other through your service.
4. **Client documentation**: A single markdown file that, when passed to an AI coding agent (e.g., Claude Code), gives the agent everything it needs to use your messaging service without any human assistance. The intuitiveness of your API, the quality of this documentation, and the overall developer and agent experience are your responsibility.

## Evaluation Criteria

You will be evaluated across the following dimensions:

| Dimension | What we are looking for |
|---|---|
| **System Design** | How you model conversations, message streaming, and delivery. Whether the design is coherent, minimal, and well-suited to agent-to-agent communication. The decisions you made and the tradeoffs you considered. |
| **Infrastructure Choices** | Whether your chosen infrastructure is used idiomatically and fits the problem. The strength of your reasoning for the choices you made. |
| **Code Quality** | Clean, readable, well-structured code. Minimal and intentional — no dead code, no unnecessary abstractions. Consistent style. |
| **Testing** | Meaningful test coverage that demonstrates correctness of the core messaging behavior. Tests should inspire confidence in the system's reliability. |
| **Reliability and Correctness** | Messages are durable. Delivery guarantees are met. Edge cases are considered (e.g., agent failures during message composition, concurrent writers, reconnection). |
| **Documentation** | Clarity and depth of design decision write-ups. Honest assessment of tradeoffs. Thoughtfulness in the "no restrictions" design discussion, particularly around infrastructure, reliability, and scale. |
| **Developer and Agent Experience** | The client documentation markdown file is self-contained and sufficient for an AI agent to use the service without human help. The API is intuitive. The onboarding path is frictionless. |
| **Live Service** | The service is deployed, reachable, and functional. At least one Claude-powered agent is running and available to converse with. We can connect our own agents as clients and exercise the core features. |
