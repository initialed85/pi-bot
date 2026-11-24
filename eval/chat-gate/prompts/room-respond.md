You decide whether the chat bot pi-chat should reply to a brand new top-level message posted in a Google Chat room shared by several humans. pi-chat is a member of the room, was not mentioned, and has not been speaking in this exchange.

Answer NO when the message is plainly part of a human-to-human exchange or needs no bot. It does when it:
- is addressed to a specific person or uses intimate terms for them ("babe did you order the balloons?", "did you get to the chemist?");
- asks a person in the room rather than the bot, signalled by "anyone", "who's", "does anyone", "you guys" or a human name - even when a bot could technically help ("anyone got a spare 4090", "who's got the house key tonight");
- asks the group for an opinion ("what do you think, do we risk it this sprint");
- asks ABOUT pi-chat while speaking to another person, or wonders out loud about its past behaviour without asking it anything ("did it ever tell us why it kept quiet", "why does it do that", "has pi-chat ever explained that") - the room is talking about the bot, not to it;
- is an errand, plan, or arrangement between people ("grab napkins when you're at dodas", "we'll leave at 7", "mum's picking us up");
- argues with, agrees with, or reacts to another person's point ("I'm telling you that change is worth the downtime", "yeah exactly");
- is banter, a joke, or a reaction with nothing to act on ("lol we eat that much food", "bruh", "gross");
- only reports the speaker's own status, movements, or availability and asks nothing ("brb taking this call", "yeah they're in the car right now", "on my way").

Answer YES when the message asks something pi-chat can answer or act on:
- a question anyone in the room would reasonably expect the bot to answer ("what's the weather tomorrow?", "how long till the sprinklers finish?", "what's on the calendar this week?");
- a request for a task, lookup, reminder, or system action, even with no question mark ("remind me to call the plumber", "check whether the dryer is still running", "add 12, 18 and 7");
- a question about pi-chat itself, its work, or its state, asked TO the bot ("are you still running that job?", "did the deploy land?");
- a report of something being wrong with a shared device, schedule, or home system, which only makes sense as a request to the bot ("the kitchen plug is off again", "the 6am roster skipped the left bank");
- a message that questions or corrects what pi-chat last said in this room, shown below - in that case the "you" and "that" in the message are pi-chat, and there is a question in it ("that seems cold, what was it overnight?", "you missed grandma in that list right?").
- pure commentary about what pi-chat reported, with no question and nothing to fix ("hmm that download looks slow"), is NO.

When genuinely unclear, answer NO for a statement and YES for a question. pi-chat staying quiet about a human conversation is acceptable; guessing wrong puts unasked-for text in front of other people.

Pi-chat's most recent answer in this room, empty if it has not spoken recently: {summary}

Message (from {sender}): "{message}"

Answer with exactly one word: YES or NO.
