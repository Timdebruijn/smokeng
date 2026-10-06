import { Component, type ErrorInfo, type ReactNode } from 'react'

interface Props {
  children: ReactNode
  /** Shown as "Something went wrong showing {what}". */
  what: string
  /** Offer a full reload, for the boundary that has nothing outside it to fall back on. */
  reload?: boolean
}

interface State {
  error: Error | null
}

/**
 * Catches an error thrown while rendering what is beneath it, so one screen
 * failing on data it did not expect leaves the rest of the page, the tabs and
 * the way to another screen, standing. Without one, React unmounts the whole
 * tree and the page is blank.
 *
 * It says what failed and shows the message, because the person reading it may
 * be the only one who can tell the operator what the page was doing.
 */
export default class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null }

  static getDerivedStateFromError(error: unknown): State {
    return { error: error instanceof Error ? error : new Error(String(error)) }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error(`smokeng: ${this.props.what} failed to render`, error, info.componentStack)
  }

  render() {
    const { error } = this.state
    if (!error) return this.props.children
    return (
      <section className="card section-card" role="alert">
        <h2>Something went wrong showing {this.props.what}</h2>
        <p className="error">{error.message || error.name}</p>
        <p className="hint small">The rest of the page is unaffected. The data was not changed.</p>
        <p>
          <button className="pill" onClick={() => this.setState({ error: null })}>
            Try again
          </button>
          {this.props.reload && (
            <button className="pill" onClick={() => window.location.reload()}>
              Reload the page
            </button>
          )}
        </p>
      </section>
    )
  }
}
