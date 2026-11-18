import { Fragment, useCallback, useState, type ReactNode } from 'react';
import { Link, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { LoadState, StatusBadge, useAccessResource } from '@/features/access/shared';
import { PageHeader } from '@/features/substrate/components/PageHeader';
import {
  passTraceApi,
  type PassTrace,
  type TraceDelivery,
  type TraceEphemeralJob,
  type TraceRequest,
  type TraceStep,
} from '@/services/brood/passTrace';
import { formatDateTimeValue } from '@/utils/format';
import { swarmPassComparisonPath, swarmPath } from './routes';
import page from '@/features/substrate/substrate.module.scss';
import local from './SwarmPassTrace.module.scss';

export function SwarmPassTracePage() {
  const { id = '', pass = '' } = useParams<{ id: string; pass: string }>();
  return <PassTraceScreen key={`${id}/${pass}`} id={id} pass={pass} />;
}

function PassTraceScreen({ id, pass }: { id: string; pass: string }) {
  const { t } = useTranslation();
  const number = Number(pass);
  const load = useCallback(() => {
    if (!Number.isSafeInteger(number) || number < 1 || String(number) !== pass)
      return Promise.reject(new Error('A canonical positive pass number is required'));
    return passTraceApi.get(id, number);
  }, [id, number, pass]);
  const resource = useAccessResource(load);
  return (
    <div className={page.page}>
      <PageHeader
        title={t('pass_trace.title', { pass })}
        meta={[
          <Link key="swarm" to={swarmPath(id)}>
            {t('pass_trace.back')}
          </Link>,
        ]}
        actions={
          <>
            {Number.isSafeInteger(number) && number > 0 && String(number) === pass && (
              <Link className="btn btn-secondary" to={swarmPassComparisonPath(id, number)}>
                {t('pass_compare.open')}
              </Link>
            )}
            <Button variant="secondary" onClick={() => void resource.refresh()}>
              {t('access.refresh')}
            </Button>
          </>
        }
      />
      <LoadState loading={resource.loading} error={resource.error} retry={resource.refresh}>
        {resource.data && <PassTraceContent trace={resource.data} />}
      </LoadState>
    </div>
  );
}

export function PassTraceContent({ trace }: { trace: PassTrace }) {
  const { t } = useTranslation();
  return (
    <div className={local.document}>
      <div className={local.intro}>
        <p className={local.hint}>{t('pass_trace.read_only')}</p>
        <a className="btn btn-secondary" href={passTraceApi.downloadUrl(trace.swarmId, trace.pass)}>
          {t('pass_trace.download')}
        </a>
      </div>
      {!trace.current && <p className={local.notice}>{t('pass_trace.historical')}</p>}
      <p className={local.goal}>{trace.goal}</p>
      <Facts
        rows={[
          [
            t('pass_trace.source'),
            <>
              <span>{trace.source.origin}</span>
              <code>{trace.source.commit}</code>
            </>,
          ],
          [
            t('pass_trace.definition'),
            <>
              {trace.definition.name} v{trace.definition.version}
              <code>{trace.definition.revision}</code>
            </>,
          ],
        ]}
      />
      {trace.gaps.length > 0 && (
        <section aria-labelledby="trace-gaps">
          <h2 id="trace-gaps">{t('pass_trace.gaps')}</h2>
          <ul className={local.gaps}>
            {trace.gaps.map((gap, index) => (
              <li key={`${gap.kind}/${gap.sequence}/${gap.requestId}/${index}`}>
                {t(`pass_trace.gap_${gap.kind}`, { defaultValue: gap.kind })}
                {gap.sequence !== undefined && <code>#{gap.sequence}</code>}
                {gap.requestId && <code>{gap.requestId}</code>}
              </li>
            ))}
          </ul>
        </section>
      )}
      <section aria-labelledby="trace-visits">
        <h2 id="trace-visits">{t('pass_trace.visits')}</h2>
        <ol className={local.visits}>
          {trace.steps.map((step) => (
            <TraceVisit key={step.sequence} step={step} />
          ))}
        </ol>
      </section>
      <section aria-labelledby="trace-requests">
        <h2 id="trace-requests">{t('pass_trace.requests')}</h2>
        {trace.requests.length === 0 ? (
          <p className={local.hint}>{t('pass_trace.requests_empty')}</p>
        ) : (
          trace.requests.map((request) => <TraceQuestion key={request.id} request={request} />)
        )}
      </section>
      {trace.unscopedActorRequests.length > 0 && (
        <section aria-labelledby="trace-unscoped">
          <h2 id="trace-unscoped">{t('pass_trace.unscoped')}</h2>
          <p className={local.notice}>{t('pass_trace.unscoped_hint')}</p>
          {trace.unscopedActorRequests.map((request) => (
            <TraceQuestion key={request.id} request={request} />
          ))}
        </section>
      )}
      <TraceDetails title={t('pass_trace.graph')} value={trace.graph} />
    </div>
  );
}

function Facts({ rows }: { rows: Array<[string, ReactNode]> }) {
  return (
    <dl className={local.facts}>
      {rows.map(([label, value]) => (
        <Fragment key={label}>
          <dt>{label}</dt>
          <dd>{value}</dd>
        </Fragment>
      ))}
    </dl>
  );
}

function KnownTime({ value }: { value?: string }) {
  const { t } = useTranslation();
  const valid =
    value !== undefined &&
    /^\d{4}-\d{2}-\d{2}T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d{1,9})?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$/.test(
      value
    ) &&
    Number.isFinite(Date.parse(value)) &&
    new Date(`${value.slice(0, 10)}T00:00:00Z`).toISOString().slice(0, 10) === value.slice(0, 10);
  if (!valid) return <span className={local.hint}>{t('pass_trace.unknown_time')}</span>;
  return <time dateTime={value}>{formatDateTimeValue(value)}</time>;
}

function TraceDetails({
  title,
  value,
  children,
}: {
  title: string;
  value?: unknown;
  children?: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  return (
    <details className={local.disclosure} onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary>{title}</summary>
      {open &&
        (children ?? (
          <pre className={local.code}>
            {typeof value === 'string' ? value : JSON.stringify(value, null, 2)}
          </pre>
        ))}
    </details>
  );
}

function TraceDeliveryView({ delivery }: { delivery: TraceDelivery }) {
  const { t } = useTranslation();
  return (
    <div className={local.delivery}>
      <pre className={local.code}>{delivery.frame}</pre>
      <h4>{t('pass_trace.attempts')}</h4>
      <ol className={local.attempts}>
        {delivery.attempts.map((attempt) => (
          <li key={attempt.number}>
            <strong>#{attempt.number}</strong>
            <code>{attempt.mode}</code>
            <KnownTime value={attempt.createdAt} />
            <span>
              {attempt.resultRetained ? attempt.outcome : t('pass_trace.missing_receipt')}
            </span>
            {attempt.detail && <p className={local.text}>{attempt.detail}</p>}
            <TraceDetails title={t('pass_trace.details')} value={attempt} />
          </li>
        ))}
      </ol>
    </div>
  );
}

function TraceEphemeralJobView({ job }: { job: TraceEphemeralJob }) {
  const { t } = useTranslation();
  const label = (key: string) => t(`pass_trace.ephemeral.${key}`);
  return (
    <article className={local.job}>
      <Facts
        rows={[
          [label('intent'), <code>{job.id}</code>],
          [label('request'), <code>{job.requestId}</code>],
          [label('pool'), <code>{job.poolUid}</code>],
          [label('queued_at'), <KnownTime value={job.queuedAt} />],
          [
            label('expected_source'),
            <>
              {job.source.origin}
              <code>{job.source.commit}</code>
            </>,
          ],
        ]}
      />
      <TraceDetails title={label('task')} value={job.task} />
      {job.failure && (
        <>
          <h4>{label('failure')}</h4>
          <Facts rows={[[label('failure_recorded'), <KnownTime value={job.failure.failedAt} />]]} />
          <TraceDetails title={label('failure_reason')} value={job.failure.reason} />
          <p className={local.hint}>{label('failure_hint')}</p>
        </>
      )}
      {job.executorFailure && (
        <>
          <h4>{label('executor_failure')}</h4>
          <Facts
            rows={[[label('failure_recorded'), <KnownTime value={job.executorFailure.failedAt} />]]}
          />
          <TraceDetails title={label('failure_reason')} value={job.executorFailure.reason} />
          <p className={local.hint}>{label('executor_failure_hint')}</p>
        </>
      )}
      {job.allocation && (
        <Facts
          rows={[
            [
              label('allocation'),
              <code>
                {job.allocation.actorUid} / {job.allocation.actorIncarnation}
              </code>,
            ],
            [label('allocated_at'), <KnownTime value={job.allocation.allocatedAt} />],
          ]}
        />
      )}
      {job.createAuthorization && (
        <Facts
          rows={[
            [label('create_authorization'), label(`create_${job.createAuthorization.state}`)],
            [
              label('authorization_created'),
              <KnownTime value={job.createAuthorization.createdAt} />,
            ],
            ...(job.createAuthorization.reservedAt
              ? [
                  [
                    label('reserved_at'),
                    <KnownTime value={job.createAuthorization.reservedAt} />,
                  ] as [string, ReactNode],
                ]
              : []),
          ]}
        />
      )}
      {job.attestation && (
        <>
          <Facts
            rows={[
              [label('source_proof'), label(`proof_${job.attestation.state}`)],
              ...(job.attestation.verifiedOrigin
                ? [
                    [
                      label('verified_source'),
                      <>
                        {job.attestation.verifiedOrigin}
                        <code>{job.attestation.verifiedCommit}</code>
                      </>,
                    ] as [string, ReactNode],
                  ]
                : []),
              [label('proof_created'), <KnownTime value={job.attestation.createdAt} />],
              [label('proof_updated'), <KnownTime value={job.attestation.updatedAt} />],
            ]}
          />
          <p className={local.hint}>{label('proof_hint')}</p>
        </>
      )}
      {job.childId && <Facts rows={[[label('child'), <code>{job.childId}</code>]]} />}
      {job.inventory && (
        <Facts
          rows={[
            [
              label('inventory'),
              <code>
                {job.inventory.uid} / {job.inventory.incarnation}
              </code>,
            ],
            ...(job.inventory.currentName
              ? [[label('current_name'), job.inventory.currentName] as [string, ReactNode]]
              : []),
            [label('phase'), <code>{job.inventory.phase}</code>],
            [label('desired'), <code>{job.inventory.desired}</code>],
            [label('operation'), <code>{job.inventory.operation}</code>],
            [label('stage'), <code>{job.inventory.stage}</code>],
            [label('inventory_updated'), <KnownTime value={job.inventory.updatedAt} />],
          ]}
        />
      )}
      {job.inventoryUnavailable && (
        <p className={local.notice}>
          {label(`inventory_${job.inventoryUnavailable}`)}. {label('inventory_hint')}
        </p>
      )}
      {job.resultRetention && (
        <>
          <h4>{label('result_retention')}</h4>
          <Facts
            rows={[
              [label('child'), <code>{job.resultRetention.childId}</code>],
              [label('child_revision'), job.resultRetention.childRevision],
              [label('child_outcome'), <code>{job.resultRetention.outcome}</code>],
              [label('parent_outcome'), <code>{job.resultRetention.parentOutcome}</code>],
              [label('retained_at'), <KnownTime value={job.resultRetention.retainedAt} />],
            ]}
          />
          <p className={local.hint}>{label('retention_hint')}</p>
        </>
      )}
    </article>
  );
}

function TraceVisit({ step }: { step: TraceStep }) {
  const { t } = useTranslation();
  return (
    <li className={local.visit}>
      <article>
        <div className={local.visitHead}>
          <h3>
            #{step.sequence} {step.nodeId}
          </h3>
          <StatusBadge tone="neutral">
            {t(`swarm_page.node_states.${step.state}`, { defaultValue: step.state })}
          </StatusBadge>
        </div>
        <Facts
          rows={[
            [t('swarm_page.started'), <KnownTime value={step.startedAt} />],
            [t('swarm_page.finished'), <KnownTime value={step.finishedAt} />],
            [t('pass_trace.actual'), step.outcome ?? t('pass_trace.not_applied')],
            ...(step.actor
              ? [
                  [
                    t('swarm_page.actor'),
                    <code>
                      {step.actor.uid} / {step.actor.incarnation}
                    </code>,
                  ] as [string, ReactNode],
                ]
              : []),
          ]}
        />
        {step.detail && <p className={local.text}>{step.detail}</p>}
        {step.report && <pre className={local.text}>{step.report}</pre>}
        {step.reportedResult && (
          <section className={local.reported}>
            <h4>{t('pass_trace.reported')}</h4>
            <p>
              <code>{step.reportedResult.outcome}</code>{' '}
              <KnownTime value={step.reportedResult.createdAt} />
            </p>
            <pre className={local.text}>{step.reportedResult.report}</pre>
          </section>
        )}
        {step.instruction && (
          <TraceDetails title={t('pass_trace.instruction')}>
            <TraceDeliveryView delivery={step.instruction} />
          </TraceDetails>
        )}
        {step.childInstructions.map((delivery) => (
          <TraceDetails
            key={delivery.id}
            title={t('pass_trace.child_instruction', { id: delivery.sourceId })}
          >
            <Facts
              rows={[
                [
                  t('swarm_page.actor'),
                  <code>
                    {delivery.actorUid} / {delivery.actorIncarnation}
                  </code>,
                ],
              ]}
            />
            <p className={local.hint}>{t('pass_trace.child_transport_hint')}</p>
            <TraceDeliveryView delivery={delivery} />
          </TraceDetails>
        ))}
        {step.ephemeralJobs.length > 0 && (
          <section aria-label={t('pass_trace.ephemeral.title')}>
            <h4>{t('pass_trace.ephemeral.title')}</h4>
            <ol className={local.jobs}>
              {step.ephemeralJobs.map((job) => (
                <li key={job.id}>
                  <TraceEphemeralJobView job={job} />
                </li>
              ))}
            </ol>
          </section>
        )}
        {step.children && <TraceDetails title={t('swarm_children.title')} value={step.children} />}
        {step.resultObservation && (
          <TraceDetails title={t('pass_trace.observation')} value={step.resultObservation} />
        )}
        {step.artifactEvidence && (
          <TraceDetails title={t('pass_trace.artifact')} value={step.artifactEvidence} />
        )}
        {step.policyCandidates.length > 0 && (
          <TraceDetails title={t('pass_trace.policy')} value={step.policyCandidates} />
        )}
        <TraceDetails title={t('pass_trace.details')} value={step} />
      </article>
    </li>
  );
}

function TraceQuestion({ request }: { request: TraceRequest }) {
  const { t } = useTranslation();
  return (
    <article className={local.question}>
      <div className={local.visitHead}>
        <h3>{request.question}</h3>
        <StatusBadge tone="neutral">
          {t(`swarm_requests.states.${request.state}`, { defaultValue: request.state })}
        </StatusBadge>
      </div>
      <p>
        <KnownTime value={request.createdAt} /> <code>{request.id}</code>
      </p>
      {request.createdDuring && (
        <p className={local.hint}>
          {t('pass_trace.request_context', {
            pass: request.createdDuring.pass,
            sequence: request.createdDuring.observedSequence,
          })}
        </p>
      )}
      {request.context && <pre className={local.text}>{request.context}</pre>}
      {request.response !== undefined && (
        <section>
          <h4>{t('swarm_requests.response')}</h4>
          <KnownTime value={request.answeredAt} />
          <pre className={local.text}>{request.response}</pre>
        </section>
      )}
      {request.deliveryHistory && (
        <TraceDetails title={t('pass_trace.instruction')}>
          <TraceDeliveryView delivery={request.deliveryHistory} />
        </TraceDetails>
      )}
      <TraceDetails title={t('pass_trace.details')} value={request} />
    </article>
  );
}
