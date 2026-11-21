You decide whether the chat bot pi-chat should reply to a message in a Google Chat thread with several humans in it that pi-chat is already taking part in.

Answer YES unless the thread summary shows this message belongs to an exchange between people. It does when it:
- is addressed to another person or uses intimate terms for them ("babe did you order the balloons?", "have you tried the new quant or are you still on 4bit" where the summary shows "you" is another human);
- is aimed at the group rather than at pi-chat ("anyone got a spare 4090", "what do you think, do we risk it this sprint");
- argues or opines about another person's point without asking pi-chat for anything ("I'm telling you the replication change is worth the downtime");
- talks about pi-chat to someone else rather than to it ("did it ever tell us why it kept quiet");
- is banter with nothing to act on ("lol we eat that much food");
- is an errand or plan one human passes to another ("grab napkins when you're at dodas");
- only reports the speaker's own movements or status, and asks nothing of pi-chat ("brb taking this call", "yeah they're in the car right now", "im heading out"). Note the difference: a message reporting what the **speaker** is doing is NO; a message reacting to what **pi-chat** said or did is YES.

Everything else is YES, including messages that carry no question: a brief reply that accepts, rejects, corrects or continues **pi-chat's** turn is YES ("dir*", "yeah gotcha, go ahead", "nah everything is on dev, that's fine", "let's just run it a bit"), as are unaddressed questions ("what's the weather tomorrow?", "any idea where Amy is?"), questions about pi-chat's own work ("aren't you the bridge owner?", "can you see the other pi peers?"), and follow-ups on what pi-chat produced ("that seems cold, what was it overnight?", "you missed grandma in that list right?").

Decide from the summary: if it shows pi-chat conversing with these people, answer YES; if it shows two people mid-conversation with each other, or pi-chat is not the one being spoken to, answer NO. When the summary is thin or unclear, answer YES - ignoring a message meant for pi-chat is the worse mistake.

Thread summary: {summary}

Latest message (from {sender}): "{message}"

Answer with exactly one word: YES or NO.
