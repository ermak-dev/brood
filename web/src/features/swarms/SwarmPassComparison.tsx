import { useCallback, useState, type FormEvent } from 'react';
import { Link, useLocation, useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { LoadState, StatusBadge, useAccessResource } from '@/features/access/shared';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import {
  passComparisonApi,
  type PassComparison,
  type ComparedVisit,
  type ComparedStep,
} from '@/services/brood/passComparison';
import { swarmPassComparisonPath, swarmPassPath, swarmPath } from './routes';
import page from '@/features/substrate/substrate.module.scss';
import trace from './SwarmPassTrace.module.scss';
import local from './SwarmPassComparison.module.scss';

function passNumber(value: string): number | undefined {
  const number = Number(value);
  return Number.isSafeInteger(number) && number > 0 && String(number) === value
    ? number
    : undefined;
}
function candidateSelection(search: string): { swarm: string; pass: number } | null | undefined {
  const params = new URLSearchParams(search);
  if (params.size === 0) return undefined;
  for (const key of params.keys())
    if (!['swarm', 'pass'].includes(key) || params.getAll(key).length !== 1) return null;
  const swarm = params.get('swarm') ?? '',
    pass = passNumber(params.get('pass') ?? '');
  return swarm && swarm.length <= 256 && pass !== undefined ? { swarm, pass } : null;
}

export function SwarmPassComparisonPage() {
  const { id = '', pass = '' } = useParams<{ id: string; pass: string }>();
  const { search } = useLocation();
  return <ComparisonScreen key={`${id}/${pass}/${search}`} id={id} pass={pass} search={search} />;
}

function ComparisonScreen({ id, pass, search }: { id: string; pass: string; search: string }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const params = new URLSearchParams(search);
  const referencePass = passNumber(pass),
    selection = candidateSelection(search);
  const [candidate, setCandidate] = useState(params.get('swarm') ?? id);
  const [candidatePass, setCandidatePass] = useState(params.get('pass') ?? '');
  const [error, setError] = useState('');
  function submit(event: FormEvent) {
    event.preventDefault();
    const number = passNumber(candidatePass);
    if (
      !id ||
      referencePass === undefined ||
      !candidate ||
      candidate.length > 256 ||
      number === undefined
    ) {
      setError(t('pass_compare.invalid'));
      return;
    }
    navigate(
      `${swarmPassComparisonPath(id, referencePass)}?${new URLSearchParams({ swarm: candidate, pass: String(number) })}`
    );
  }
  return (
    <div className={page.page}>
      <PageHeader
        title={t('pass_compare.title')}
        meta={[
          <Link
            key="trace"
            to={referencePass === undefined ? swarmPath(id) : swarmPassPath(id, referencePass)}
          >
            {t('pass_compare.back')}
          </Link>,
        ]}
      />
      <div className={trace.document}>
        <p className={trace.hint}>{t('pass_compare.read_only')}</p>
        <form className={local.selection} onSubmit={submit}>
          <Input
            name="candidateSwarm"
            label={t('pass_compare.swarm')}
            value={candidate}
            maxLength={256}
            required
            onChange={(event) => setCandidate(event.target.value)}
          />
          <Input
            name="candidatePass"
            label={t('pass_compare.pass')}
            value={candidatePass}
            inputMode="numeric"
            pattern="[1-9][0-9]*"
            maxLength={19}
            required
            onChange={(event) => setCandidatePass(event.target.value)}
          />
          <Button type="submit">{t('pass_compare.submit')}</Button>
        </form>
        {(error || selection === null || referencePass === undefined) && (
          <p role="alert">{error || t('pass_compare.invalid')}</p>
        )}
        {!selection && <p className={trace.hint}>{t('pass_compare.empty')}</p>}
        {selection && referencePass !== undefined && (
          <ComparisonResult
            reference={id}
            referencePass={referencePass}
            candidate={selection.swarm}
            candidatePass={selection.pass}
          />
        )}
      </div>
    </div>
  );
}

function ComparisonResult({
  reference,
  referencePass,
  candidate,
  candidatePass,
}: {
  reference: string;
  referencePass: number;
  candidate: string;
  candidatePass: number;
}) {
  const { t } = useTranslation();
  const load = useCallback(
    () => passComparisonApi.get(reference, referencePass, candidate, candidatePass),
    [reference, referencePass, candidate, candidatePass]
  );
  const resource = useAccessResource(load);
  return (
    <div>
      <Button variant="secondary" onClick={() => void resource.refresh()}>
        {t('access.refresh')}
      </Button>
      <LoadState loading={resource.loading} error={resource.error} retry={resource.refresh}>
        {resource.data && <PassComparisonContent comparison={resource.data} />}
      </LoadState>
    </div>
  );
}

export function PassComparisonContent({ comparison }: { comparison: PassComparison }) {
  const { t } = useTranslation();
  const [allOpen, setAllOpen] = useState(false);
  return (
    <div className={trace.document}>
      <div className={trace.intro}>
        <p className={trace.notice}>{t('pass_compare.alignment')}</p>
        <a
          className="btn btn-secondary"
          href={passComparisonApi.downloadUrl(
            comparison.reference.swarmId,
            comparison.reference.pass,
            comparison.candidate.swarmId,
            comparison.candidate.pass
          )}
        >
          {t('pass_compare.download')}
        </a>
      </div>
      <dl className={trace.facts}>
        {(['reference', 'candidate'] as const).map((side) => (
          <div className={local.fact} key={side}>
            <dt>{t(`pass_compare.${side}`)}</dt>
            <dd>
              <Link to={swarmPassPath(comparison[side].swarmId, comparison[side].pass)}>
                <code>
                  {comparison[side].swarmId} #{comparison[side].pass}
                </code>
              </Link>
            </dd>
          </div>
        ))}
        {(['sameGoal', 'sameSource', 'sameDefinition'] as const).map((key, index) => (
          <div className={local.fact} key={key}>
            <dt>{t(`pass_compare.${['same_goal', 'same_source', 'same_definition'][index]}`)}</dt>
            <dd>{t(comparison[key] ? 'common.yes' : 'common.no')}</dd>
          </div>
        ))}
        {(['stepCount', 'childTaskCount'] as const).map((key, index) => (
          <div className={local.fact} key={key}>
            <dt>{t(`pass_compare.${index === 0 ? 'visits' : 'children'}`)}</dt>
            <dd>{t('pass_compare.counts', { ...comparison[key] })}</dd>
          </div>
        ))}
      </dl>
      <ol className={trace.visits}>
        {comparison.steps.map((row) => (
          <ComparisonVisit key={row.position} row={row} />
        ))}
      </ol>
      <details
        className={trace.disclosure}
        onToggle={(event) => setAllOpen(event.currentTarget.open)}
      >
        <summary>{t('pass_compare.details')}</summary>
        {allOpen && <pre className={trace.code}>{JSON.stringify(comparison, null, 2)}</pre>}
      </details>
    </div>
  );
}

function ComparisonVisit({ row }: { row: ComparedVisit }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  return (
    <li className={trace.visit}>
      <details className={trace.disclosure} onToggle={(event) => setOpen(event.currentTarget.open)}>
        <summary>
          <strong>{t('pass_compare.position', { position: row.position })}</strong>{' '}
          <code>{row.reference?.nodeId ?? t('pass_compare.missing')}</code> /{' '}
          <code>{row.candidate?.nodeId ?? t('pass_compare.missing')}</code>{' '}
          <StatusBadge tone="neutral">
            {t('pass_compare.comparison_label', { label: t('pass_compare.actual_outcome') })}:{' '}
            {t(`pass_compare.${row.outcome}`)}
          </StatusBadge>
        </summary>
        {open && (
          <div>
            <dl className={trace.facts}>
              {(['outcome', 'report', 'reportedOutcome', 'reportedReport'] as const).map(
                (key, index) => (
                  <div className={local.fact} key={key}>
                    <dt>
                      {t('pass_compare.comparison_label', {
                        label: t(
                          `pass_compare.${['actual_outcome', 'actual_report', 'reported_outcome', 'reported_report'][index]}`
                        ),
                      })}
                    </dt>
                    <dd>{t(`pass_compare.${row[key]}`)}</dd>
                  </div>
                )
              )}
              <div className={local.fact}>
                <dt>{t('pass_compare.duration_delta')}</dt>
                <dd>{row.durationDeltaMillis ?? t('pass_compare.unknown')}</dd>
              </div>
            </dl>
            <div className={local.pair}>
              <ComparedStepView label={t('pass_compare.reference')} step={row.reference} />
              <ComparedStepView label={t('pass_compare.candidate')} step={row.candidate} />
            </div>
          </div>
        )}
      </details>
    </li>
  );
}

function ComparedStepView({ label, step }: { label: string; step?: ComparedStep }) {
  const { t } = useTranslation();
  if (!step)
    return (
      <section>
        <h3>{label}</h3>
        <p>{t('pass_compare.missing')}</p>
      </section>
    );
  return (
    <section>
      <h3>
        {label} #{step.sequence} <code>{step.nodeId}</code>
      </h3>
      <p>
        <StatusBadge tone="neutral">
          {t(`swarm_page.node_states.${step.state}`, { defaultValue: step.state })}
        </StatusBadge>
      </p>
      <dl className={trace.facts}>
        <div className={local.fact}>
          <dt>{t('pass_compare.actual_outcome')}</dt>
          <dd>{step.outcome ?? t('pass_compare.unknown')}</dd>
        </div>
        <div className={local.fact}>
          <dt>{t('pass_compare.reported_outcome')}</dt>
          <dd>{step.reportedOutcome ?? t('pass_compare.unknown')}</dd>
        </div>
        <div className={local.fact}>
          <dt>{t('pass_compare.duration')}</dt>
          <dd>{step.durationMillis ?? t('pass_compare.unknown')}</dd>
        </div>
        <div className={local.fact}>
          <dt>{t('pass_compare.children')}</dt>
          <dd>{step.childTasks}</dd>
        </div>
      </dl>
      <h4>{t('pass_compare.actual_report')}</h4>
      <pre className={trace.text}>{step.report ?? t('pass_compare.unknown')}</pre>
      <h4>{t('pass_compare.reported_report')}</h4>
      <pre className={trace.text}>{step.reportedReport ?? t('pass_compare.unknown')}</pre>
    </section>
  );
}
