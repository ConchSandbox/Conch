package state

type SandboxRecord struct {
	SandboxID                     string `json:"sandbox_id"`
	Namespace                     string `json:"namespace"`
	CheckpointHeadTemplateID      string `json:"checkpoint_head_template_id"`
	CheckpointHeadBootIndexDigest string `json:"checkpoint_head_boot_index_digest"`
}
