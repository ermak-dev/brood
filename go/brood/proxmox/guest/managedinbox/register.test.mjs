// FEATURE: Deliver one claimed supervisor inbox offer through native prompt intake.
// SPEC: Poll only in an interactive terminal using the fixed, bounded helper;
// submit the exact valid text once, even when the submit callback fails.
// IMPLEMENTATION: Defer to the mod clock and stop polling on any result other
// than a clean empty response. Validate the whole offer before submitting it.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const modulePath = new URL("./marketplace/plugins/supervisor-inbox/hooks/register.js", import.meta.url);
async function harness(run, submit = async () => ({})) {
  const source = await readFile(modulePath);
  const { register } = await import(`data:text/javascript;base64,${source.toString("base64")}`);
  const hooks = new Map();
  const pending = new Set();
  const requests = [];
  const submissions = [];
  register((event, handler) => hooks.set(event, handler));

  const $ = {
    clock: {
      after(delay, callback) {
        assert.ok(Number.isFinite(delay) && delay > 0 && delay <= 5000);
        pending.add(callback);
        return () => pending.delete(callback);
      },
    },
    process: {
      async run(argv, opts) {
        requests.push({ argv, opts });
        return run();
      },
    },
    prompt: {
      async submit(request) {
        submissions.push(request);
        return submit();
      },
    },
  };
  return {
    async start(input = { isInteractive: true, surface: "terminal" }) {
      const started = { cwd: "/workspace/project" };
      const hook = hooks.get("session.start");
      assert.ok(hook, "missing session.start hook");
      assert.equal(await hook($, input, async (event) => {
        assert.equal(event, input);
        return started;
      }), started);
    },
    async drain() {
      for (let step = 0; pending.size; step++) {
        assert.ok(step < 100, "poll did not exhaust its budget");
        const callback = pending.values().next().value;
        pending.delete(callback);
        await callback();
      }
    },
    requests,
    submissions,
  };
}

const exactText = "Keep literal `shell` syntax,\nquotes, <>&, and Юникод unchanged.\n";
const validOffer = JSON.stringify({ deliveryId: "inbox:291-01", text: exactText })
  .replace(/[<>&]/g, (char) => `\\u00${char.charCodeAt(0).toString(16)}`) + "\n";
const noOffer = { exitCode: 1, stdout: "" };

function offer(stdout = validOffer) {
  return { exitCode: 0, stdout, stderr: "", isStdoutTruncated: false, isStderrTruncated: false };
}

test("polls after start and submits one valid offer verbatim", async () => {
  let calls = 0;
  const mod = await harness(() => ++calls === 1 ? noOffer : offer());
  await mod.start();
  await mod.start();
  assert.deepEqual(mod.requests, [], "session.start must not wait for helper work");
  await mod.drain();
  assert.equal(mod.requests.length, 2);
  for (const { argv, opts } of mod.requests) {
    assert.deepEqual(argv, ["/opt/brood/bin/brood-guest", "--brood-guest-inbox"]);
    assert.equal(opts.stdin, '{"operation":"take"}');
    assert.equal(opts.cwd, "/");
    assert.ok(opts.timeoutMs > 3000 && opts.timeoutMs <= 5000);
  }
  assert.deepEqual(mod.submissions, [{ text: exactText }]);
});

test("does not poll for a non-interactive or non-terminal session", async () => {
  const mod = await harness(() => offer());
  await mod.start({ isInteractive: false, surface: null });
  await mod.start({ isInteractive: true, surface: "desktop" });
  await mod.drain();
  assert.deepEqual(mod.requests, []);
  assert.deepEqual(mod.submissions, []);
});

test("poll budget runs out when no offer is available", async () => {
  const mod = await harness(() => noOffer);
  await mod.start();
  await mod.drain();
  assert.ok(mod.requests.length > 1 && mod.requests.length < 100);
  assert.deepEqual(mod.submissions, []);
});

test("rejects malformed, oversized, or truncated helper output", async () => {
  for (const result of [
    offer("not JSON"),
    offer("null\n"),
    offer('{"deliveryId":"bad id","text":"run"}\n'),
    offer('{"deliveryId":"valid","text":""}\n'),
    offer('{"deliveryId":"valid","text":"run","permit":"unexpected"}\n'),
    offer('{"deliveryId":"valid","deliveryId":"valid","text":"run"}\n'),
    offer(validOffer + validOffer),
    offer(JSON.stringify({ deliveryId: "valid", text: "λ".repeat(65536) }) + "\n"),
    { ...offer(), isStdoutTruncated: true },
  ]) {
    const mod = await harness(() => result);
    await mod.start();
    await mod.drain();
    assert.deepEqual(mod.submissions, []);
  }
});

test("does not poll again after an ambiguous helper timeout", async () => {
  const mod = await harness(() => { throw Error("helper timeout"); });
  await mod.start();
  await mod.drain();
  assert.equal(mod.requests.length, 1);
  assert.deepEqual(mod.submissions, []);
});

test("does not retry, including reentrant start, when submission fails", async () => {
  for (const result of [() => { throw Error("submission timeout"); }, () => ({ drop: "denied" })]) {
    const mod = await harness(() => offer(), async () => {
      await mod.start();
      return result();
    });
    await mod.start();
    await mod.drain();
    assert.equal(mod.requests.length, 1);
    assert.deepEqual(mod.submissions, [{ text: exactText }]);
  }
});
