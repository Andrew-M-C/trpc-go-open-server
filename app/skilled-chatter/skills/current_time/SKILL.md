---
name: current_time
description: Returns the host system date and time by running the date command inside the skill workspace.
---

# Current time

When the user asks for the current time or date:

1. Call `skill_load` for skill `current_time` if you have not loaded it yet for this task.
2. Call `skill_run` with:
   - `skill`: `current_time`
   - `command`: `date` for local time, or `date -u` if the user wants UTC.

Report the command's stdout to the user.
