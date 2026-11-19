const maxOfferBytes = 128 * 1024; // relay.MaxMessageBytes
const maxPolls = 12;
const pollDelayMs = 1000;

// Разбираем ровно две строковые пары. JSON.parse декодирует каждый токен
// отдельно, чтобы повтор ключа (в том числе через \u-escape) не потерялся.
const quoted = /"(?:[^"\\]|\\[\s\S])*"/.source;
const ws = /[ \t\r\n]*/.source;
const offerPattern = new RegExp(
  `^${ws}\\{${ws}(${quoted})${ws}:${ws}(${quoted})${ws},${ws}` +
  `(${quoted})${ws}:${ws}(${quoted})${ws}\\}${ws}$`,
);

function offerText(stdout) {
  if (typeof stdout !== "string" || stdout.length > maxOfferBytes ||
      new TextEncoder().encode(stdout).length > maxOfferBytes) return;
  const match = offerPattern.exec(stdout);
  if (!match) return;
  const [key1, value1, key2, value2] = match.slice(1).map((token) => JSON.parse(token));
  let deliveryId, text;
  if (key1 === "deliveryId" && key2 === "text") {
    [deliveryId, text] = [value1, value2];
  } else if (key1 === "text" && key2 === "deliveryId") {
    [text, deliveryId] = [value1, value2];
  } else {
    return;
  }
  if (deliveryId.length === 0 || deliveryId.length > 128 || /[^A-Za-z0-9_.:-]/.test(deliveryId) ||
      text.length === 0 || /[\uD800-\uDFFF]/u.test(text)) return;
  return text;
}

async function poll($, remaining) {
  try {
    const result = await $.process.run(["/opt/brood/bin/brood-guest", "--brood-guest-inbox"], {
      cwd: "/",
      stdin: '{"operation":"take"}',
      timeoutMs: 4000,
    });
    // В 2.1.310 флаги не описаны: лимит helper проверяем независимо от них.
    if (result.isStdoutTruncated || result.isStderrTruncated ||
        (result.stderr !== undefined && result.stderr !== "")) return;
    // Повторяем только чистый пустой ответ. Ошибка helper после claim
    // может выглядеть как отсутствие данных, но пишет причину в stderr.
    if (result.exitCode === 1 && result.stdout === "") {
      if (remaining > 1) $.clock.after(pollDelayMs, () => poll($, remaining - 1));
      return;
    }
    if (result.exitCode !== 0) return;
    const text = offerText(result.stdout);
    if (text !== undefined) await $.prompt.submit({ text });
  } catch {
    // Не повторяем вызов с неопределённым исходом. Не выводим payload или ошибку.
  }
}

export const register = (on) => {
  let scheduled = false;
  on("session.start", async ($, event, next) => {
    const started = await next(event);
    if (!scheduled && event.isInteractive === true && event.surface === "terminal") {
      scheduled = true;
      $.clock.after(pollDelayMs, () => poll($, maxPolls));
    }
    return started;
  });
};
