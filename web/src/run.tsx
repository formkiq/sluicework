import { useEffect, useState } from 'react'
import { ArrowLeft, RotateCcw, StopCircle, Terminal } from 'lucide-react'
import { api, base, idPath } from './api'
import { attemptOf, runStatus, terminal, when, platformName } from './model'
import type { Machine, Run, Log } from './model'
import { Badge, ErrorMessage, Heading, Panel } from './ui'
type Call = {
  attempt_id: string
  kind: string
  model: string
  duration_ms: number
  input_tokens?: number
  output_tokens?: number
  status: string
  error?: string
}
export function RunDetail({
  id,
  machines,
  back,
  change,
}: {
  id: string
  machines: Machine[]
  back: () => void
  change: () => void
}) {
  const [run, setRun] = useState<Run>(),
    [logs, setLogs] = useState<Log[]>([]),
    [calls, setCalls] = useState<Call[]>([])
  const [selected, setSelected] = useState(''),
    [error, setError] = useState(''),
    [stream, setStream] = useState('Connecting'),
    [revision, bump] = useState(0),
    [busy, setBusy] = useState(false)
  const path = idPath('/runs', id)
  useEffect(() => {
    let active = true,
      source: EventSource | undefined,
      timer = 0
    const entries = new Map<number, Log>()
    let cursor = 0
    const append = (incoming: Log[]) => {
      for (const line of incoming) {
        entries.set(line.id, line)
        cursor = Math.max(cursor, line.id)
      }
      if (active) setLogs([...entries.values()].sort((a, b) => a.id - b.id))
    }
    const stored = async () => {
      while (active) {
        const batch = await api<Log[]>(path + '/logs?after=' + cursor)
        if (!active) return
        append(batch || [])
        if (!batch || batch.length < 1000) return
      }
    }
    const usage = async () => {
      const result = await api<Call[]>(path + '/calls')
      if (active) setCalls(result || [])
    }
    const receive = async (r: Run) => {
      if (!active) return
      setRun(r)
      if (terminal(r.state)) source?.close()
      if (r.job.type === 'ai') await usage()
      if (terminal(r.state)) {
        await stored()
        if (active) {
          source?.close()
          setStream('Stored logs')
        }
      }
    }
    const connect = async () => {
      try {
        const r = await api<Run>(path)
        if (!active) return
        await stored()
        await receive(r)
        if (!active || terminal(r.state)) return
        source = new EventSource(base + path + '/events?after=' + cursor)
        source.onopen = () => {
          if (active) {
            setStream('Live logs')
            setError('')
          }
        }
        source.addEventListener('log', (e) => {
          if (active) append([JSON.parse((e as MessageEvent).data) as Log])
        })
        source.addEventListener('run', (e) => {
          void receive(JSON.parse((e as MessageEvent).data)).catch((err) => {
            if (active) setError(err.message)
          })
        })
        source.onerror = () => {
          source?.close()
          if (active) {
            setStream('Reconnecting')
            timer = window.setTimeout(connect, 2000)
          }
        }
      } catch (err) {
        if (active) {
          setError((err as Error).message)
          setStream('Disconnected')
          timer = window.setTimeout(connect, 5000)
        }
      }
    }
    void connect()
    return () => {
      active = false
      source?.close()
      clearTimeout(timer)
    }
  }, [path, revision])
  const attempt = run?.attempts?.find((at) => at.id === selected) || (run && attemptOf(run))
  const call = calls.find((x) => x.attempt_id === attempt?.id)
  const invoke = async (action: string) => {
    setBusy(true)
    setError('')
    try {
      await api(path + '/' + action, 'POST')
      setSelected('')
      bump((n) => n + 1)
      change()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }
  const attempts = run?.attempts || []
  return (
    <>
      <button className="link back" onClick={back}>
        <ArrowLeft size={15} />
        Run history
      </button>
      {error && <ErrorMessage>{error}</ErrorMessage>}
      {!run ? (
        <p role="status">Loading run…</p>
      ) : (
        <>
          <Heading
            title={run.job.name}
            description={'Run ' + id}
            action={
              <div className="actions">
                <Badge value={runStatus(run)} />
                {terminal(run.state) ? (
                  <button className="primary" disabled={busy} onClick={() => invoke('retry')}>
                    <RotateCcw size={15} />
                    Retry
                  </button>
                ) : (
                  <button
                    className="subtle-danger"
                    disabled={busy || !!attemptOf(run)?.cancelRequested}
                    onClick={() => invoke('cancel')}
                  >
                    <StopCircle size={15} />
                    Cancel run
                  </button>
                )}
              </div>
            }
          />
          <div className="run-summary">
            <div>
              <span>Template revision</span>
              <b>{run.job.revision}</b>
            </div>
            <div>
              <span>Triggered by</span>
              <b>{run.trigger}</b>
            </div>
            <div>
              <span>Queued at</span>
              <b>{when(run.createdAt)}</b>
            </div>
            <div>
              <span>Target role</span>
              <b>{run.job.role}</b>
            </div>
            <div>
              <span>Required platform</span>
              <b>{platformName(run.job)}</b>
            </div>
          </div>
          <Panel
            title="Attempts"
            action={
              attempts.length > 1 && (
                <select
                  aria-label="Select attempt"
                  value={attempt?.id}
                  onChange={(e) => setSelected(e.target.value)}
                >
                  {[...attempts].reverse().map((at) => (
                    <option key={at.id} value={at.id}>
                      Attempt {at.number} · {at.state}
                    </option>
                  ))}
                </select>
              )
            }
          >
            {attempt && (
              <>
                <div className="attempt-line">
                  <b>Attempt {attempt.number}</b>
                  <Badge
                    value={
                      attempt.state === 'running' && attempt.cancelRequested
                        ? 'cancellation_requested'
                        : attempt.state
                    }
                  />
                  {attempt.outcomeUnknown && <span className="warning">Outcome unknown</span>}
                </div>
                <dl className="detail-grid attempt-details">
                  <div>
                    <dt>Assigned machine</dt>
                    <dd>
                      {machines.find((m) => m.id === attempt.machineId)?.name ||
                        attempt.machineId ||
                        'Awaiting placement'}
                    </dd>
                  </div>
                  <div>
                    <dt>Started</dt>
                    <dd>{when(attempt.startedAt)}</dd>
                  </div>
                  <div>
                    <dt>Completed</dt>
                    <dd>{when(attempt.endedAt)}</dd>
                  </div>
                </dl>
                {attempt.error && (
                  <div className="padded">
                    <ErrorMessage>{attempt.error}</ErrorMessage>
                  </div>
                )}
              </>
            )}
          </Panel>
          {call && (
            <Panel title="Provider request">
              <dl className="detail-grid attempt-details">
                <div>
                  <dt>Provider / model</dt>
                  <dd>
                    {call.kind} / {call.model}
                  </dd>
                </div>
                <div>
                  <dt>Duration</dt>
                  <dd>{call.duration_ms} ms</dd>
                </div>
                <div>
                  <dt>Tokens (input / output)</dt>
                  <dd>
                    {call.input_tokens ?? '—'} / {call.output_tokens ?? '—'}
                  </dd>
                </div>
              </dl>
              {call.error && (
                <div className="padded">
                  <ErrorMessage>{call.error}</ErrorMessage>
                </div>
              )}
            </Panel>
          )}
          <Panel
            title="Execution log"
            action={
              <span className="minor">
                <Terminal size={14} />
                {stream}
              </span>
            }
          >
            {(attempt?.logsTruncated ||
              logs.some((l) => l.attemptId === attempt?.id && l.truncated)) && (
              <div className="warning">Log truncated at the configured size limit.</div>
            )}
            <pre className="console-log" tabIndex={0}>
              {logs
                .filter((l) => l.attemptId === attempt?.id)
                .map((l) => l.text)
                .join('') || 'No log output.'}
            </pre>
          </Panel>
          <Panel title="Result">
            {attempt?.outputTruncated && (
              <div className="warning">Result truncated at the output limit.</div>
            )}
            <pre className="result" tabIndex={0}>
              {attempt?.output ||
                (run.job.discardOutput
                  ? 'Result retention is disabled for this template.'
                  : terminal(run.state)
                    ? 'No result recorded.'
                    : 'Awaiting completion.')}
            </pre>
          </Panel>
        </>
      )}
    </>
  )
}
