package app

// The skills are the shared foundation (CLAUDE.md): some users run only the
// skills in Claude Code, without Labmill, so every rule a typed tool applies
// must also be written down in a generic skill, with the shell commands a
// Claude Code user runs. toolSkills records, per tool, the skill sections
// that describe its domain logic; TestToolSkillSections fails when a listed
// section is missing from the embedded skills, or when a registered tool is
// in neither toolSkills nor noDomainLogic. Change a tool's rules → update the
// sections listed here (and add one when a new rule has no home yet).

// skillSection names one section of an embedded generic skill (skills/).
type skillSection struct {
	Skill  string // skill name, e.g. "slurm-user"
	File   string // "SKILL.md", or a reference path such as "references/auth.md"
	Anchor string // GitHub-style heading anchor, without '#'
}

// toolSkills maps each typed tool with domain logic to the skill sections
// that describe that logic. Only generic skills are listed: site profiles
// are private and may be missing from a build.
var toolSkills = map[string][]skillSection{
	"list_clusters": {
		{"slurm-user", "references/slurm.md", "partitions-accounts-and-qos"},
	},
	"cluster_info": {
		{"slurm-user", "references/slurm.md", "partitions-accounts-and-qos"},
		{"slurm-user", "references/slurm.md", "requesting-gpus"},
	},
	"find_local_data": {
		{"slurm-user", "references/from-your-laptop.md", "two-folders-data-and-code"},
		{"slurm-user", "references/from-your-laptop.md", "what-claude-may-read"},
	},
	"preview_file": {
		{"slurm-user", "references/from-your-laptop.md", "what-claude-may-read"},
		{"research-computing", "references/environments.md", "read-the-readme-and-the-build-files-before-you-guess"},
	},
	"stage_data": {
		{"slurm-user", "references/from-your-laptop.md", "two-folders-data-and-code"},
		{"slurm-user", "references/from-your-laptop.md", "what-claude-may-read"},
		{"slurm-user", "references/from-your-laptop.md", "when-there-is-data-but-no-code"},
	},
	"draft_job": {
		{"slurm-user", "references/slurm.md", "a-job-script"},
		{"slurm-user", "references/slurm.md", "never-point---output--error-at-tmp"},
		{"slurm-user", "references/slurm.md", "create-the---output--error-folder-first"},
		{"slurm-user", "references/slurm.md", "the-code-has-no-run-script"},
		{"slurm-user", "references/from-your-laptop.md", "where-things-go-on-the-cluster"},
		{"slurm-user", "references/from-your-laptop.md", "paths-from-the-laptop-dont-exist-on-the-cluster"},
	},
	"job_status": {
		{"slurm-user", "references/slurm.md", "checking-a-job-without-waiting"},
	},
	"cluster_jobs": {
		{"slurm-user", "references/slurm.md", "your-queue-and-recent-jobs"},
	},
	"job_logs": {
		{"slurm-user", "references/slurm.md", "checking-a-job-without-waiting"},
		{"slurm-user", "references/slurm.md", "a-job-that-ended-badly"},
	},
	"diagnose_job": {
		{"slurm-user", "references/failures.md", "collect-the-facts"},
		{"slurm-user", "references/failures.md", "read-the-numbers"},
		{"slurm-user", "references/failures.md", "match-the-log-line"},
		{"slurm-user", "references/failures.md", "remember-the-cause"},
	},
	"ask_user": {
		{"slurm-user", "references/from-your-laptop.md", "show-before-you-move-or-submit"},
	},
	"upload_data": {
		{"slurm-user", "references/from-your-laptop.md", "show-before-you-move-or-submit"},
		{"slurm-user", "references/from-your-laptop.md", "where-things-go-on-the-cluster"},
		{"slurm-user", "references/from-your-laptop.md", "copying-files-with-rclone"},
		{"slurm-user", "references/from-your-laptop.md", "check-what-would-be-overwritten"},
		{"research-computing", "references/data-transfer.md", "rclone"},
	},
	"submit_job": {
		{"slurm-user", "references/slurm.md", "create-the---output--error-folder-first"},
		{"slurm-user", "references/from-your-laptop.md", "show-before-you-move-or-submit"},
		{"slurm-user", "references/slurm.md", "a-job-script"},
	},
	"cancel_job": {
		{"slurm-user", "references/from-your-laptop.md", "show-before-you-move-or-submit"},
		{"slurm-user", "references/slurm.md", "commands"},
	},
	"fetch_results": {
		{"slurm-user", "references/from-your-laptop.md", "show-before-you-move-or-submit"},
		{"slurm-user", "references/from-your-laptop.md", "getting-results-back"},
		{"research-computing", "references/data-transfer.md", "rclone"},
		{"research-computing", "references/filesystems-and-storage.md", "integrity-checksums"},
	},

	"s3_credentials_to_cluster": {
		{"research-computing", "references/data-transfer.md", "copy-one-aws-profile-to-a-login-node"},
	},

	"cleanup_job": {
		{"slurm-user", "references/placement-and-preemption.md", "cleaning-up-after-a-verified-download"},
		{"slurm-user", "references/from-your-laptop.md", "getting-results-back"},
	},

	"remote_command": {
		{"slurm-user", "references/slurm.md", "commands"},
		{"slurm-user", "references/slurm.md", "checking-a-job-without-waiting"},
		{"slurm-user", "references/slurm.md", "your-queue-and-recent-jobs"},
	},

	"add_cluster": {
		{"research-computing", "references/ssh.md", "a-cluster-at-another-institution"},
	},
	"set_cluster_account": {
		{"research-computing", "references/ssh.md", "the-account-name-on-a-new-cluster"},
	},

	// Pending (pendingTools): tools other milestones are adding.
	"prepare_job": {
		{"slurm-user", "references/slurm.md", "a-job-script"},
		{"slurm-user", "references/from-your-laptop.md", "where-things-go-on-the-cluster"},
	},
}

// noDomainLogic lists registered tools that apply no rule a skill needs to
// describe (they only read Labmill's own state or manage the conversation).
var noDomainLogic = []string{
	"list_jobs",    // the jobs in Labmill's local store
	"update_todos", // pending (pendingTools): the to-do list tool of M28
}

// pendingTools are names in toolSkills or noDomainLogic that other
// milestones are adding (M28: a to-do tool; a possible prepare_job). The drift test tolerates them being absent from Tools.
var pendingTools = []string{"prepare_job", "update_todos"}
