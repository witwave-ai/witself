// Support-notification rendering, extracted so it is testable under plain
// node (index.js itself imports workerd-only packages). Keep the presentation
// constants and helpers byte-aligned with index.js's shared email template.

const EMAIL_FONT = "-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Oxygen,Ubuntu,Cantarell,sans-serif";
const EMAIL_TEXT = "#0f172a";
const EMAIL_MUTED = "#64748b";
const EMAIL_BG = "#f4f5f7";
const EMAIL_CARD = "#ffffff";
const EMAIL_BORDER = "#eef0f4";

function escapeHTML(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function cliBlock(cmd) {
  return `<div style="background:${EMAIL_BG};border:1px solid ${EMAIL_BORDER};border-radius:6px;padding:14px 18px;font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:13px;color:${EMAIL_TEXT};overflow-x:auto;margin:16px 0;">${escapeHTML(cmd)}</div>`;
}

function renderEmail({ title, preheader, body }) {
  const preheaderMarkup = preheader
    ? `<div style="display:none;font-size:1px;color:${EMAIL_BG};line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;">${escapeHTML(preheader)}</div>`
    : "";
  return `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${escapeHTML(title)}</title></head><body style="margin:0;padding:0;background:${EMAIL_BG};">${preheaderMarkup}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:${EMAIL_BG};padding:40px 12px;"><tr><td align="center"><table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0" style="max-width:600px;background:${EMAIL_CARD};border-radius:8px;box-shadow:0 1px 3px rgba(0,0,0,0.05);"><tr><td style="padding:28px 40px;border-bottom:1px solid ${EMAIL_BORDER};"><div style="font-family:${EMAIL_FONT};font-size:19px;font-weight:600;color:${EMAIL_TEXT};letter-spacing:-0.02em;">Witself</div></td></tr><tr><td style="padding:32px 40px;font-family:${EMAIL_FONT};font-size:15px;line-height:1.65;color:${EMAIL_TEXT};"><h1 style="margin:0 0 20px;font-size:22px;font-weight:600;letter-spacing:-0.01em;color:${EMAIL_TEXT};">${escapeHTML(title)}</h1>${body}</td></tr><tr><td style="padding:20px 40px 28px;border-top:1px solid ${EMAIL_BORDER};font-family:${EMAIL_FONT};font-size:13px;line-height:1.55;color:${EMAIL_MUTED};">Sent by Witself. If you didn't expect this email, you can safely ignore it.</td></tr></table></td></tr></table></body></html>`;
}

// renderSupportEmail returns { subject, text, html } for one of the three
// customer-facing support notifications. Assistant replies never attribute
// the message to the human admin credential used by the operator-run service.
export function renderSupportEmail(
  kind,
  authorKind,
  adminHandle,
  accountID,
  ticketID,
  body,
) {
  const assistantReply = kind === "reply" && authorKind === "assistant";
  const visibleBody = assistantReply && adminHandle
    ? String(body).split(String(adminHandle)).join("[support assistant]")
    : body;
  const showCmd = `witself account support show --ticket ${ticketID}`;
  const replyCmd = `witself account support reply --ticket ${ticketID} --stdin`;
  const openCmd = `witself account support open`;

  const preview = (() => {
    if (!visibleBody) return "";
    const clean = visibleBody.replace(/\s+/g, " ").trim();
    if (clean.length <= 400) return clean;
    return clean.slice(0, 400) + "…";
  })();

  const variants = {
    reply: {
      title: "Support replied to your ticket",
      subject: `Support replied to your ticket ${ticketID}`,
      opening: authorKind === "assistant"
        ? "The Witself support assistant replied to your ticket."
        : `${adminHandle} from Witself support replied to your ticket.`,
      cta: replyCmd,
      ctaLabel: "Reply",
    },
    resolved: {
      title: "Your support ticket was marked resolved",
      subject: `Ticket ${ticketID} marked resolved`,
      opening: `${adminHandle} from Witself support marked your ticket as resolved.`,
      cta: `witself account support close --ticket ${ticketID}`,
      ctaLabel: "Close it out",
    },
    closed: {
      title: "Your support ticket was closed",
      subject: `Ticket ${ticketID} closed`,
      opening: `${adminHandle} from Witself support closed your ticket.`,
      cta: openCmd,
      ctaLabel: "Open a new ticket",
    },
  };
  const v = variants[kind];
  const previewHTML = preview
    ? `<blockquote style="margin:0 0 20px;padding:12px 16px;border-left:3px solid ${EMAIL_BORDER};color:${EMAIL_MUTED};font-size:14px;">${escapeHTML(preview)}</blockquote>`
    : "";
  const html = renderEmail({
    title: v.title,
    preheader: v.opening,
    body: `
      <p style="margin:0 0 16px;">${escapeHTML(v.opening)}</p>
      <p style="margin:0 0 8px;color:${EMAIL_MUTED};font-size:13px;">Account · Ticket</p>
      <div style="font-family:ui-monospace,SFMono-Regular,'SF Mono',Menlo,Consolas,monospace;font-size:14px;color:${EMAIL_TEXT};margin:0 0 20px;">${escapeHTML(accountID)} · ${escapeHTML(ticketID)}</div>
      ${previewHTML}
      <p style="margin:0 0 8px;">View the full thread:</p>
      ${cliBlock(showCmd)}
      <p style="margin:20px 0 8px;">${escapeHTML(v.ctaLabel)}:</p>
      ${cliBlock(v.cta)}
    `,
  });
  const textPreview = preview ? `\n\n> ${preview}\n` : "";
  const text = `${v.opening}\n\nAccount: ${accountID}\nTicket:  ${ticketID}${textPreview}\n\nView the thread:\n\n  ${showCmd}\n\n${v.ctaLabel}:\n\n  ${v.cta}\n`;
  return { subject: v.subject, text, html };
}
