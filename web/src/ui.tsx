import { cloneElement, useEffect, useId, useRef, useState } from 'react'
import type { ReactNode, ReactElement } from 'react'
import { AlertCircle, ArrowRight, Box, ChevronRight, LoaderCircle, X } from 'lucide-react'
import { stateNames } from './model'

export function Brand() {
  return (
    <span className="brand">
      <span className="brand-mark" aria-hidden="true">
        <i />
        <i />
        <i />
      </span>
      Sluicework
    </span>
  )
}
export function Badge({ value }: { value: string }) {
  return (
    <span className={'badge ' + value}>
      <i />
      {({ accepting: 'Ready', building: 'Building', draining: 'Draining', paused: 'Paused' } as Record<string, string>)[value] || stateNames[value] || value.replace(/_/g, ' ')}
    </span>
  )
}
export function Panel({
  title,
  action,
  children,
  className = '',
}: {
  title: string
  action?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <section className={'panel ' + className}>
      <header className="panel-head">
        <h2>{title}</h2>
        {action}
      </header>
      {children}
    </section>
  )
}
export function LinkButton({ children, onClick }: { children: ReactNode; onClick: () => void }) {
  return (
    <button className="link" onClick={onClick}>
      {children} <ChevronRight size={14} />
    </button>
  )
}
export function Empty({
  title,
  children,
  action,
}: {
  title: string
  children?: ReactNode
  action?: ReactNode
}) {
  return (
    <div className="empty">
      <Box size={22} strokeWidth={1.4} />
      <strong>{title}</strong>
      {children && <p>{children}</p>}
      {action}
    </div>
  )
}
export function Heading({
  title,
  description,
  action,
}: {
  title: string
  description: string
  action?: ReactNode
}) {
  return (
    <header className="heading">
      <div>
        <h1>{title}</h1>
        <p>{description}</p>
      </div>
      {action}
    </header>
  )
}
export function Field({
  label,
  children,
  hint,
}: {
  label: string
  children: ReactElement
  hint?: string
}) {
  const id = useId()
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      {cloneElement(children, { id, 'aria-describedby': hint ? id + '-hint' : undefined })}
      {hint && <small id={id + '-hint'}>{hint}</small>}
    </div>
  )
}
export function ErrorMessage({ children }: { children: ReactNode }) {
  return (
    <div className="error-message" role="alert">
      <AlertCircle size={16} />
      <span>{children}</span>
    </div>
  )
}
export function Dialog({
  title,
  children,
  close,
  wide,
}: {
  title: string
  children: ReactNode
  close: () => void
  wide?: boolean
}) {
  const ref = useRef<HTMLDialogElement>(null)
  const titleId = useId()
  useEffect(() => {
    const dialog = ref.current
    dialog?.showModal()
    return () => dialog?.close()
  }, [])
  return (
    <dialog
      ref={ref}
      aria-labelledby={titleId}
      onCancel={(e) => {
        e.preventDefault()
        close()
      }}
      className={wide ? 'wide' : ''}
    >
      <header className="dialog-head">
        <h2 id={titleId}>{title}</h2>
        <button className="icon" aria-label="Close dialog" onClick={close}>
          <X size={20} />
        </button>
      </header>
      {children}
    </dialog>
  )
}
export function Form({
  children,
  submit,
  label = 'Save',
  close,
  danger,
  cancelLabel = 'Close',
}: {
  children: ReactNode
  submit: (data: FormData) => Promise<void>
  label?: string
  close: () => void
  danger?: boolean
  cancelLabel?: string
}) {
  const [busy, setBusy] = useState(false),
    [error, setError] = useState('')
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault()
        if (busy) return
        const data = new FormData(e.currentTarget)
        setBusy(true)
        setError('')
        try {
          await submit(data)
        } catch (err) {
          setError((err as Error).message)
        } finally {
          setBusy(false)
        }
      }}
    >
      <div className="form-body">
        {children}
        {error && <ErrorMessage>{error}</ErrorMessage>}
      </div>
      <footer className="dialog-foot">
        <button type="button" disabled={busy} onClick={(e) => { e.currentTarget.form?.reset(); setError(''); close() }}>
          {cancelLabel}
        </button>
        <button disabled={busy} className={danger ? 'danger' : 'primary'}>
          {busy ? <LoaderCircle size={15} className="spin" /> : <ArrowRight size={15} />} {label}
        </button>
      </footer>
    </form>
  )
}
export const val = (d: FormData, k: string) => String(d.get(k) ?? '').trim()
export const checked = (d: FormData, k: string) => d.has(k)
