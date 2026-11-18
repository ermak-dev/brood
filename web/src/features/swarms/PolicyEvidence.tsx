import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { LoadState } from '@/features/access/shared';
import { errorMessage } from '@/services/brood/http';
import { policyEvidenceApi, type PolicyCandidate } from '@/services/brood/policyEvidence';
import page from './SwarmPage.module.scss';
import local from './SwarmRequests.module.scss';

export function PolicyEvidence({ swarmId, requestId }: { swarmId: string; requestId: string }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const [result, setResult] = useState<{
    key: string;
    data: PolicyCandidate | null;
    error: string;
  } | null>(null);
  const key = `${swarmId}/${requestId}`;
  useEffect(() => {
    if (!open) return;
    let current = true;
    setResult({ key, data: null, error: '' });
    void policyEvidenceApi.detail(swarmId, requestId).then(
      (data) => {
        if (current) setResult({ key, data, error: '' });
      },
      (error: unknown) => {
        if (current) setResult({ key, data: null, error: errorMessage(error) });
      }
    );
    return () => {
      current = false;
    };
  }, [open, attempt, key, swarmId, requestId]);
  const data = result?.key === key ? result.data : null;
  const error = result?.key === key ? result.error : '';
  return (
    <details className={local.context} onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary className={local.contextSummary}>{t('swarm_requests.policy_evidence')}</summary>
      {open && (
        <LoadState
          loading={!data && !error}
          error={error}
          retry={async () => {
            setAttempt((value) => value + 1);
          }}
        >
          {data && (
            <>
              <p className={page.hint}>{t('swarm_requests.policy_evidence_hint')}</p>
              <pre className={local.text}>{JSON.stringify(data, null, 2)}</pre>
            </>
          )}
        </LoadState>
      )}
    </details>
  );
}
