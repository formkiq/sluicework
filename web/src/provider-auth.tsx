import { useState } from 'react'
import { ConnectionEditor } from './forms'
import { Panel } from './ui'
import { cloudSetup } from './provider-info'
export type PlanModel = { id: string; name: string }
export function ProviderOptions({ admin, changed }: { admin: boolean; changed: () => void }) {
  const [editor, setEditor] = useState('')
  return <>
    <Panel title="Shared provider API keys" className="provider-access">
      <p>Administrators manage shared API-key connections. Personal ChatGPT subscriptions run only in the private agent below; subscription credentials are never stored here.</p>
      <div className="provider-options">{Object.entries(cloudSetup).map(([kind, provider]) => <article key={kind}><h3>{provider.name} API key</h3><p>{provider.hint}</p><div className="actions"><button disabled={!admin} onClick={() => setEditor(kind)}>Add API key</button><a href={provider.keys} target="_blank" rel="noreferrer">Get API key ↗</a></div></article>)}</div>
      {!admin && <p>Ask an administrator to change shared API-key connections.</p>}
    </Panel>
    {editor && <ConnectionEditor initialKind={editor} close={() => setEditor('')} saved={() => { setEditor(''); changed() }} />}
  </>
}
