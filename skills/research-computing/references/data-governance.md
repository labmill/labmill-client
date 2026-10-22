# Data governance: what may go onto a shared platform

Read this before moving a dataset onto a cluster or an object
store, especially when it will be copied (a bucket copy, a staging folder). What
a given institution has actually decided is in your site skill; this file is the
generic checklist.

## Which data class is it?

Different classes carry different obligations. Ask which applies before anything
is uploaded:

| Class | Typical examples |
|---|---|
| Student records (FERPA) | grades, enrollment, advising data |
| Protected health information (HIPAA/PHI) | clinical records, identifiable patient data |
| Controlled unclassified (CUI) and export-controlled (ITAR/EAR) | defense, certain engineering and technology data |
| Federal tax information | IRS-derived data |
| Human-subjects data under IRB | surveys, interviews, identifiable participant data |
| Genomic data | human sequence data, often with consent limits |
| Sponsor-confidential research data | data under an NDA or a contract clause |

If you are unsure whether a dataset is regulated, **treat it as regulated** and
ask your institution's information security office before uploading. For
transition and benchmarking work, use non-regulated or de-identified data, and
test with a small subset first.

## Questions a platform must answer

- **Who approves** a dataset for the platform, and on what evidence?
- **Where does it live, for how long, and who deletes it?** Copies (a staging
  bucket, a per-job folder, a results folder) are easy to create and easy to
  forget; list them and remove what is no longer needed
  (`rclone ls`, `du -sh`, `ls -la` on the staging folders).
- **Who can read it?** Project membership, folder permissions (`ls -ld`,
  `getfacl`), bucket policy.
- **Is access audited?** Know whether reads and writes are logged and who sees
  the log.

A mount of the original folder adds no second copy, but the same class question
applies to the folder you mount. What the site has decided: see your site skill.
