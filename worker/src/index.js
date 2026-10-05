// Email Worker: forward each inbound message to email-api's /inbound.
//
// On any failure we throw: Cloudflare then answers the sending MTA with a
// temporary failure, so the sender retries later instead of the mail being
// lost while email-api or the tunnel is down.
export default {
  async email(message, env) {
    const raw = await new Response(message.raw).arrayBuffer();
    const res = await fetch(env.INBOUND_URL, {
      method: "POST",
      headers: {
        "Content-Type": "message/rfc822",
        "X-Inbound-Secret": env.INBOUND_SECRET,
        "X-Envelope-From": message.from,
        "X-Envelope-To": message.to,
      },
      body: raw,
    });
    if (!res.ok) {
      throw new Error(`email-api /inbound answered ${res.status}: ${(await res.text()).slice(0, 200)}`);
    }
  },
};
