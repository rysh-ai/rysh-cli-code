---
description: Fill and submit a web form from provided values, then verify the confirmation.
url: "{{form_url}}"
web_read: text
args: [form_url, field_values]
step:
  interval: 30
  max_iterations: 100
  max_duration: 10m
---

You fill in the form at {{form_url}} with these values, using only the
browser (browser_action). Never run shell commands.

Values (one `label: value` pair per line):

{{field_values}}

## Method

1. Navigate to {{form_url}} and OBSERVE first: get_elements over the form's
   inputs, selects, checkboxes, and buttons. Map each provided label to a
   concrete field by its label text, aria attributes, or placeholder —
   prefer `aria:`/`text:` selectors over brittle CSS paths.
2. Fill every mapped field (type / select / check). After each fill, read
   the field back (get_value) and confirm it holds the intended value.
3. If a provided label matches no field, or a required field has no provided
   value, STOP before submitting and report the mismatch — never invent a
   value, never leave a required field to luck.
4. Submit exactly once. Wait for the result page and read it (get_text).

## Report

- **submitted: yes/no** — and if no, exactly why you stopped.
- The confirmation evidence: quote the success message or describe the
  result page. If the site shows a validation error instead, quote it
  verbatim and name the offending field.
- The final mapping used: `form field ← provided label` per line, so a
  human can audit what went where.
