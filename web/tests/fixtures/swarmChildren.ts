import type { ChildGroup, ChildGroupSummary, ChildTask } from '@/services/brood/children';

export const childSummary = (over: Partial<ChildGroupSummary> = {}): ChildGroupSummary => ({
  parentSequence: 3,
  pass: 1,
  parentUid: 'parent-original',
  parentIncarnation: 2,
  mode: 'all',
  total: 2,
  quorum: 0,
  state: 'open',
  revision: 1,
  interrupted: false,
  registered: 1,
  open: 2,
  started: 0,
  ...over,
});

export const childTask = (over: Partial<ChildTask> = {}): ChildTask => ({
  id: 'child-1',
  requestId: 'independent-1',
  role: 'review',
  executorUid: 'executor-original',
  executorIncarnation: 4,
  state: 'pending',
  delivery: 'queued',
  deliveryDetail: '',
  task: 'Inspect <script>not executable</script>\nPreserve source bytes.',
  outcome: '',
  report: '',
  revision: 1,
  createdAt: '2026-11-15T10:00:00Z',
  finishedAt: '',
  ...over,
});

export const childGroup = (over: Partial<ChildGroup> = {}): ChildGroup => ({
  ...childSummary(),
  readiness: 'pending',
  abortedBy: '',
  abortedAt: '',
  children: [childTask()],
  ...over,
});
