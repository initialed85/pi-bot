# pi-bot

## Chat replies are read on a phone

Ed and Amy receive every pi-chat message on a mobile screen. Anything this session sends into Chat
(`chat_post`, and any reply the bridge relays) follows these rules. Code and terminal answers to Ed
directly are not covered by them.

- Lead with the answer in one line. Supporting numbers go underneath, never in front of it.
- Data goes in a table or a nested bullet list. Never a paragraph that has to be re-read to find a figure.
- Keep lines short: a phone wraps around 40 characters, so rows and bullets must survive wrapping.
- No preamble, no restating the question, no "in summary" tail, no sign-off.
- Terse without losing meaning: drop hedges and adjectives, keep every number, name, commit and decision.
- One subject per message. If a message carries three unrelated findings, it should have been three posts.
- A number that is arithmetic rather than measured says so, and a claim that has been retracted is corrected
  in the same day it was made.
