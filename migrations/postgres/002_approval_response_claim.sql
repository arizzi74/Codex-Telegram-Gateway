ALTER TABLE approvals
    ADD COLUMN response_command_id UUID REFERENCES commands(command_id);

CREATE UNIQUE INDEX approvals_response_command_id_idx
    ON approvals (response_command_id)
    WHERE response_command_id IS NOT NULL;
