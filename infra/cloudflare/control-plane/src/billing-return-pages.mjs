// Public, value-free landing pages for provider-hosted billing flows.
//
// These routes deliberately do not authenticate a browser, inspect a Stripe
// session, read account state, or mutate anything. Hosted Checkout and the
// billing portal can return to them even when the local Agent Console is not
// running; the AI/CLI that opened the provider flow remains responsible for
// reading authenticated billing status and for every Witself-side mutation.

export const BILLING_RETURN_PATHS = Object.freeze({
  success: "/billing/success",
  cancelled: "/billing/cancelled",
  portal: "/billing/portal-return",
});

const pages = Object.freeze({
  [BILLING_RETURN_PATHS.success]: Object.freeze({
    title: "Checkout returned to Witself",
    message:
      "Return to the AI or terminal that opened checkout. It can read your " +
      "authenticated billing status. This page has no account session, does " +
      "not change anything, and is not confirmation that a plan or payment " +
      "completed.",
  }),
  [BILLING_RETURN_PATHS.cancelled]: Object.freeze({
    title: "Checkout closed",
    message:
      "No billing action is performed by this page. Return to the AI or " +
      "terminal that opened checkout to read the current authenticated " +
      "billing status or retry the hosted flow.",
  }),
  [BILLING_RETURN_PATHS.portal]: Object.freeze({
    title: "Billing portal closed",
    message:
      "Return to the AI or terminal that opened the portal. It can read your " +
      "authenticated billing status. This page has no account session and " +
      "cannot confirm or change billing state.",
  }),
});

const securityHeaders = Object.freeze({
  "Cache-Control": "no-store, max-age=0",
  "Content-Security-Policy":
    "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; " +
    "form-action 'none'; frame-ancestors 'none'",
  "Cross-Origin-Opener-Policy": "same-origin",
  "Cross-Origin-Resource-Policy": "same-origin",
  "Permissions-Policy":
    "camera=(), geolocation=(), microphone=(), payment=(), usb=()",
  "Referrer-Policy": "no-referrer",
  "X-Content-Type-Options": "nosniff",
  "X-Frame-Options": "DENY",
  "X-Robots-Tag": "noindex, nofollow, noarchive",
});

function html(page) {
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="robots" content="noindex,nofollow,noarchive">
  <title>${page.title} - Witself</title>
  <style>
    :root { color-scheme: light dark; }
    body { font-family: system-ui, sans-serif; max-width: 38rem; margin: 15vh auto 0; padding: 0 1.5rem; line-height: 1.5; }
    h1 { font-size: 1.5rem; line-height: 1.2; }
    code { background: color-mix(in srgb, currentColor 10%, transparent); border-radius: .25rem; padding: .15rem .35rem; }
  </style>
</head>
<body>
  <main>
    <h1>${page.title}</h1>
    <p>${page.message}</p>
    <p>Read-only status command: <code>witself billing show</code></p>
    <p>You can close this tab.</p>
  </main>
</body>
</html>
`;
}

// billingReturnResponse returns null for every non-return path so the
// Worker's existing API/container routing is unchanged. Query parameters are
// ignored and never reflected: configured return URLs are canonical and
// query-free, and a caller-controlled value can never become page content.
export function billingReturnResponse(request, url = new URL(request.url)) {
  const page = pages[url.pathname];
  if (!page) return null;

  if (request.method !== "GET" && request.method !== "HEAD") {
    return new Response("method not allowed\n", {
      status: 405,
      headers: {
        ...securityHeaders,
        Allow: "GET, HEAD",
        "Content-Type": "text/plain; charset=utf-8",
      },
    });
  }

  return new Response(request.method === "HEAD" ? null : html(page), {
    status: 200,
    headers: {
      ...securityHeaders,
      "Content-Type": "text/html; charset=utf-8",
    },
  });
}
