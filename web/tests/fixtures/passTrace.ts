import { childGroup, childTask } from './swarmChildren';

export function passTrace(over: Record<string, unknown> = {}) {
  return {
    version: 1,
    swarmId: 'swarm',
    pass: 1,
    goal: 'Keep <script>untrusted text</script> intact.',
    source: { origin: 'https://git.example.org/project', commit: 'a'.repeat(40) },
    definition: { name: 'develop', version: 2, revision: 'snapshot-v2' },
    graph: 'apiVersion: brood.dev/v1alpha1\nname: develop\n',
    current: true,
    execution: 'running',
    steps: [
      {
        sequence: 1,
        nodeId: 'build',
        state: 'inflight',
        actor: { uid: 'builder', incarnation: 1, currentName: 'worker' },
        requestId: 'instruction-1',
        attestedCommit: 'a'.repeat(40),
        startedAt: '2026-11-17T00:00:00Z',
        reportedResult: {
          requestId: 'instruction-1',
          actorUid: 'builder',
          actorIncarnation: 1,
          outcome: 'success',
          report: 'Reported work is not applied yet.',
          createdAt: '2026-11-17T00:01:00Z',
        },
        resultObservation: { id: 'observation-1', state: 'observed', revision: 1 },
        artifactEvidence: {
          commit: 'b'.repeat(40),
          baseCommit: 'a'.repeat(40),
          producerUid: 'builder',
          incarnation: 1,
          paths: ['go/<script>not executable</script>.go'],
        },
        policyCandidates: [],
        childInstructions: [],
        ephemeralJobs: [],
      },
    ],
    requests: [
      {
        id: 'actor-question',
        swarmId: 'swarm',
        origin: 'actor',
        actorUid: 'builder',
        actorIncarnation: 1,
        kind: 'text',
        question: 'Preserve <img src=x onerror=alert(1)>?',
        context: '',
        options: [],
        state: 'answered',
        createdAt: '2026-11-17T00:00:30Z',
        answeredAt: '2026-11-17T00:01:10Z',
        answeredByUid: 'owner',
        response: 'Keep all migration rows.',
        delivery: 'unconfirmed',
        createdDuring: { pass: 1, observedSequence: 1 },
      },
    ],
    unscopedActorRequests: [],
    gaps: [{ kind: 'instruction_not_retained', sequence: 1 }],
    ...over,
  };
}

export function ephemeralJob(over: Record<string, unknown> = {}) {
  return {
    id: 'job-queued',
    requestId: 'ephemeral-request',
    task: '  Preserve <script>queued task</script>\nΩ  ',
    poolUid: 'pool-historical',
    queuedAt: '2026-11-17T00:00:00Z',
    source: { origin: 'https://git.example.org/pinned', commit: 'c'.repeat(40) },
    ...over,
  };
}

export function allocatedJob(over: Record<string, unknown> = {}) {
  return ephemeralJob({
    id: 'job-allocated',
    allocation: {
      actorUid: 'removed-executor',
      actorIncarnation: 4,
      allocatedAt: '2026-11-17T00:01:00Z',
    },
    createAuthorization: {
      state: 'reserved',
      createdAt: '2026-11-17T00:01:00Z',
      reservedAt: '2026-11-17T00:02:00Z',
    },
    attestation: {
      state: 'attested',
      verifiedOrigin: 'https://git.example.org/pinned',
      verifiedCommit: 'c'.repeat(40),
      createdAt: '2026-11-17T00:03:00Z',
      updatedAt: '2026-11-17T00:04:00Z',
    },
    childId: 'child-1',
    inventoryUnavailable: 'not_retained',
    resultRetention: {
      childId: 'child-1',
      childRevision: 3,
      outcome: 'success',
      parentOutcome: 'success',
      retainedAt: '2026-11-17T00:05:00Z',
    },
    ...over,
  });
}

export function ephemeralTrace(jobs: unknown = [ephemeralJob(), allocatedJob()]) {
  const value = childTrace();
  return { ...value, steps: [{ ...value.steps[0], ephemeralJobs: jobs }] };
}

export const childFrame = ' { "body": "  child\\n<script>data</script> Ω  ", "to":"old-name" }\n';

export function childInstruction(over: Record<string, unknown> = {}) {
  return traceDelivery({
    id: 'delivery:child-1',
    sourceKind: 'child',
    sourceId: 'child-1',
    actorUid: 'removed-executor',
    actorIncarnation: 4,
    frame: childFrame,
    attempts: [
      {
        number: 1,
        mode: 'send',
        createdAt: '2026-11-17T00:00:00Z',
        deadlineAt: '2026-11-17T00:01:00Z',
        resultRetained: true,
        outcome: 'unconfirmed',
        detail: 'Send has no confirmed receipt.',
      },
      {
        number: 2,
        mode: 'probe',
        createdAt: '2026-11-17T00:02:00Z',
        deadlineAt: '2026-11-17T00:03:00Z',
        resultRetained: false,
      },
    ],
    ...over,
  });
}

export function childTrace(childInstructions: unknown = [childInstruction()]) {
  const value = passTrace();
  return {
    ...value,
    steps: [
      {
        ...value.steps[0],
        instruction: traceDelivery(),
        childInstructions,
        children: childGroup({
          parentSequence: 1,
          started: 1,
          open: 1,
          children: [
            childTask({
              executorUid: 'removed-executor',
              state: 'completed',
              delivery: 'unconfirmed',
              outcome: 'success',
              report: 'Independent completed report.',
            }),
          ],
        }),
      },
    ],
  };
}

export function traceDelivery(over: Record<string, unknown> = {}) {
  return {
    id: 'delivery:one',
    sourceKind: 'step',
    sourceId: '1',
    actorUid: 'builder',
    actorIncarnation: 1,
    createdAt: '2026-11-17T00:00:00Z',
    probeOnly: false,
    frame: '{"to":"worker","body":"<script>data</script>","from":"swarm","waitSec":0}',
    attempts: [
      {
        number: 1,
        mode: 'send',
        createdAt: '2026-11-17T00:00:00Z',
        deadlineAt: '2026-11-17T00:01:00Z',
        resultRetained: false,
      },
    ],
    ...over,
  };
}
