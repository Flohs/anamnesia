import { Component } from "react";
import type { ErrorInfo, ReactNode } from "react";

import { Alert, Code } from "@/components/ui/Alert";

interface ErrorBoundaryProps {
  /** What failed, in the reader's terms: "Shape", "FIG. 02", "Memory". */
  label: string;
  /** Changing this clears a caught error, so navigating away recovers. */
  resetKey?: string;
  children: ReactNode;
}

interface ErrorBoundaryState {
  message: string | null;
}

/**
 * Keeps one broken panel from taking the console with it.
 *
 * A render that throws unmounts the whole React tree, and on a dark page that
 * reads as a black screen with nothing to act on. Every crash this console has
 * had came from a field the server declined to send, and the cost each time
 * was the entire page rather than the one table that could not draw it.
 *
 * The message is shown rather than swallowed: the reader is usually the person
 * who can fix it, and "reading 'slice' of undefined" names the shape of the
 * problem far better than an apology does. It stays a class because
 * componentDidCatch has no hook equivalent.
 */
export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  override state: ErrorBoundaryState = { message: null };

  static getDerivedStateFromError(error: unknown): ErrorBoundaryState {
    return { message: error instanceof Error ? error.message : String(error) };
  }

  override componentDidUpdate(previous: ErrorBoundaryProps) {
    if (previous.resetKey !== this.props.resetKey && this.state.message !== null) {
      this.setState({ message: null });
    }
  }

  override componentDidCatch(error: unknown, info: ErrorInfo) {
    // Left in the console on purpose: this is the only place the stack
    // survives now that the crash no longer reaches the top level.
    console.error(`[${this.props.label}] render failed`, error, info.componentStack);
  }

  override render() {
    if (this.state.message === null) return this.props.children;

    return (
      <Alert code="UI" tone="bad" title={`${this.props.label} could not be drawn`}>
        Something in this panel threw while rendering, so it has been left out rather than taking
        the rest of the page with it. The console holds the stack. <Code>{this.state.message}</Code>
      </Alert>
    );
  }
}
