# pi-bot

## Chat replies are read on a phone

Ed and Amy receive every pi-chat message on a mobile screen. Anything this session sends into Chat
(`chat_post`, and any reply the bridge relays) follows these rules. Code and terminal answers to Ed
directly are not covered by them.

These rules are not cosmetic. Measured on 2026-11-27, **five** of Ed's direct messages arrived between
07:33Z and 08:05Z while this session was doing long tool work: two of them started **1h02m** and **1h19m**
late, and three were still queued past their 30-minute ping (`agent started the turn ... waited`, and
`turn still queued`, in the bridge log 07:33Z to 08:53Z).

- Lead with the answer in one line. Supporting numbers go underneath, never in front of it.
- Data goes in a table or a nested bullet list. Never a paragraph that has to be re-read to find a figure.
- Keep lines short: a phone wraps around 40 characters, so rows and bullets must survive wrapping.
- No preamble, no restating the question, no "in summary" tail, no sign-off.
- Terse without losing meaning: drop hedges and adjectives, keep every number, name, commit and decision.
- **A default Chat answer is under ~120 words.** Long answers travel with long turns, and a turn is the unit
  of queueing: while one is running, every later message waits. Go long only when the content is a report,
  and then say in the first line that it is one.
- One subject per message. If a message carries three unrelated findings, it should have been three posts.
- **A Chat message is answered before deep work starts, not after.** Investigation that takes many tool calls
  belongs in a scheduled job or a background job; a waiting person gets a short answer first and a detail
  message when the work is done.
- A number that is arithmetic rather than measured says so, and a claim that has been retracted is corrected
  in the same day it was made.
