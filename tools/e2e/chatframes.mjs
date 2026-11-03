// Every frame the chat socket (/ws) can carry, decoded the way the browser
// does (web/src/chat.js). Shared by the raw-socket harnesses so there is one
// decoder to keep in step with the server.
//
// It reads EVERY string of every frame it knows. The socket's strings are
// interned: a client that skips a frame carrying one falls behind the
// server's dictionary and decodes every later frame wrong, quietly. (That
// was the messaging harness's 23 failures when activity frames arrived.)
import { Op } from '../../web/src/wire.js';

const text = (b) => new TextDecoder().decode(b);

// decodeChatFrame fills f (which already has f.op) from the reader.
export function decodeChatFrame(h, r, f = { op: h.op }) {
  switch (h.op) {
    case Op.ConvMessage: {
      const m = {
        conversationId: r.str(),
        id: r.str(),
        senderId: r.str(),
        senderName: r.str(),
        createdAt: Number(r.uvarint()),
        deleted: r.bool(),
        edited: r.bool(),
        forwarded: r.bool(),
        system: r.bool(),
      };
      r.bits(4);
      m.body = text(r.blob());
      m.replyTo = r.str();
      if (m.replyTo) {
        m.quoted = { senderId: r.str(), senderName: r.str(), deleted: r.bool() };
        r.bits(7);
        m.quoted.body = text(r.blob());
      }
      const n = Number(r.uvarint());
      m.reactions = [];
      for (let i = 0; i < n && !r.err; i++) {
        const emoji = r.str();
        const count = Number(r.uvarint());
        const k = Number(r.uvarint());
        const who = [];
        for (let j = 0; j < k && !r.err; j++) who.push(r.str());
        m.reactions.push({ emoji, count, who });
      }
      const nAtt = Number(r.uvarint());
      m.attachments = [];
      for (let i = 0; i < nAtt && !r.err; i++) m.attachments.push({ id: r.str(), name: r.str(), mime: r.str(), size: Number(r.uvarint()) });
      const nMen = Number(r.uvarint());
      m.mentions = [];
      for (let i = 0; i < nMen && !r.err; i++) m.mentions.push(r.str());
      m.priority = r.str();
      m.meta = text(r.blob());
      const nOpt = Number(r.uvarint());
      if (nOpt > 0) {
        const counts = [];
        const voters = [];
        for (let i = 0; i < nOpt && !r.err; i++) {
          counts.push(Number(r.uvarint()));
          const k = Number(r.uvarint());
          const who = [];
          for (let j = 0; j < k && !r.err; j++) who.push(r.str());
          voters.push(who);
        }
        m.poll = { counts, voters, total: Number(r.uvarint()) };
      }
      f.msg = m;
      break;
    }
    case Op.Status:
      f.status = { who: r.str(), status: r.str(), note: r.str(), until: Number(r.uvarint()) };
      f.status.auto = r.bool();
      f.status.online = r.bool();
      r.bits(6);
      f.status.lastSeen = Number(r.uvarint());
      break;
    case Op.Presence:
      f.presence = { who: r.str(), on: r.bool() };
      r.bits(7);
      f.presence.lastSeen = Number(r.uvarint());
      break;
    case Op.ReadCursor:
      f.read = { convId: r.str(), who: r.str(), at: Number(r.uvarint()) };
      break;
    case Op.Typing:
      f.typing = { convId: r.str(), who: r.str(), whoId: r.str(), on: r.bool() };
      r.bits(7);
      break;
    case Op.Ring:
      f.ring = { callerId: r.str(), callerName: r.str(), room: r.str() };
      break;
    case Op.RingAnswer:
      f.ringAnswer = { calleeId: r.str(), calleeName: r.str(), room: r.str(), accepted: r.bool() };
      r.bits(7);
      break;
    case Op.Activity: {
      const kind = r.bits(2);
      r.bits(6);
      if (kind === 0) {
        const s = [];
        for (let i = 0; i < 12; i++) s.push(r.str());
        const [id, k, actorId, actorName, conversationId, conversationKind, conversationTitle, teamId, teamName, messageId, rootId, emoji] = s;
        const at = Number(r.uvarint());
        const read = r.bool();
        r.bits(7);
        f.activity = { kind: 'item', item: { id, kind: k, actorId, actorName, conversationId, conversationKind, conversationTitle, teamId, teamName, messageId, rootId, emoji, at, read, snippet: text(r.blob()) }, unread: Number(r.uvarint()) };
      } else if (kind === 1) {
        const n = Number(r.uvarint());
        const ids = [];
        for (let i = 0; i < n && !r.err; i++) ids.push(r.str());
        f.activity = { kind: 'read', ids, unread: Number(r.uvarint()) };
      } else {
        f.activity = { kind: 'gone', id: r.str(), unread: Number(r.uvarint()) };
      }
      break;
    }
    default:
      f.unknown = true;
  }
  f.decodeError = !!r.err;
  return f;
}
