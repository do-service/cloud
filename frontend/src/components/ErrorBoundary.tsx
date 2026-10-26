import { Button } from "@mantine/core";
import { IconMoodSad } from "@tabler/icons-react";
import { Component, type ErrorInfo, type ReactNode } from "react";
import { useLocation } from "react-router-dom";
import { t } from "../i18n";
import { EmptyState } from "./States";

type Props = { children: ReactNode; resetKey: string };
type State = { failed: boolean; key: string };

// Catches a render error below it so one broken screen shows a way out
// instead of a blank page. Errors are only caught in render, not in event
// handlers or queries — those have their own states.
class Boundary extends Component<Props, State> {
  state: State = { failed: false, key: this.props.resetKey };

  static getDerivedStateFromError(): Partial<State> {
    return { failed: true };
  }

  // Going to another page leaves the broken one behind: it gets a fresh try.
  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    return props.resetKey === state.key ? null : { failed: false, key: props.resetKey };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error(error, info.componentStack);
  }

  render() {
    if (!this.state.failed) return this.props.children;
    return (
      <EmptyState
        icon={<IconMoodSad size={36} stroke={1.5} />}
        color="red"
        title={t("crash.title")}
        description={t("crash.description")}
        action={
          <Button variant="light" onClick={() => window.location.reload()}>
            {t("crash.reload")}
          </Button>
        }
      />
    );
  }
}

// The router has no error boundary of its own (no data router), so the
// routes are wrapped in this one, reset by the location.
export default function ErrorBoundary({ children }: { children: ReactNode }) {
  const location = useLocation();
  return <Boundary resetKey={location.key}>{children}</Boundary>;
}
