use std::sync::{Arc, RwLock};

use constant_time_eq::constant_time_eq;
use tonic::metadata::MetadataValue;
use tonic::service::Interceptor;
use tonic::{Request, Status};

use crate::constants::AGENT_TOKEN_METADATA_KEY;

#[derive(Default)]
pub struct AgentAuth {
    token: RwLock<String>,
}

impl AgentAuth {
    pub fn set_token(&self, token: &str) {
        if let Ok(mut guard) = self.token.write() {
            *guard = token.trim().to_string();
        }
    }

    pub fn verify_value(
        &self,
        got: Option<&MetadataValue<tonic::metadata::Ascii>>,
    ) -> Result<(), Status> {
        let expected = self.token.read().map(|v| v.clone()).unwrap_or_default();
        if expected.is_empty() {
            return Err(Status::unauthenticated("agent token is not initialized"));
        }
        let Some(got) = got else {
            return Err(Status::unauthenticated("agent token is required"));
        };
        let got = got
            .to_str()
            .map_err(|_| Status::unauthenticated("agent token is invalid"))?;
        if got.is_empty() {
            return Err(Status::unauthenticated("agent token is required"));
        }
        if got.len() > 4096 {
            return Err(Status::unauthenticated("agent token is invalid"));
        }
        if !constant_time_eq(got.as_bytes(), expected.as_bytes()) {
            return Err(Status::unauthenticated("agent token is invalid"));
        }
        Ok(())
    }
}

#[derive(Clone)]
pub struct AuthInterceptor {
    pub auth: Arc<AgentAuth>,
}

impl Interceptor for AuthInterceptor {
    fn call(&mut self, req: Request<()>) -> Result<Request<()>, Status> {
        self.auth
            .verify_value(req.metadata().get(AGENT_TOKEN_METADATA_KEY))?;
        Ok(req)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tonic::metadata::MetadataMap;

    #[test]
    fn auth_requires_initialized_token() {
        let auth = AgentAuth::default();
        let mut metadata = MetadataMap::new();
        metadata.insert(AGENT_TOKEN_METADATA_KEY, "secret".parse().unwrap());

        let err = auth
            .verify_value(metadata.get(AGENT_TOKEN_METADATA_KEY))
            .expect_err("uninitialized token should be rejected");
        assert_eq!(err.code(), tonic::Code::Unauthenticated);
    }

    #[test]
    fn auth_accepts_matching_token_and_rejects_wrong_token() {
        let auth = AgentAuth::default();
        auth.set_token("secret");

        let mut ok = MetadataMap::new();
        ok.insert(AGENT_TOKEN_METADATA_KEY, "secret".parse().unwrap());
        assert!(auth.verify_value(ok.get(AGENT_TOKEN_METADATA_KEY)).is_ok());

        let mut wrong = MetadataMap::new();
        wrong.insert(AGENT_TOKEN_METADATA_KEY, "wrong".parse().unwrap());
        let err = auth
            .verify_value(wrong.get(AGENT_TOKEN_METADATA_KEY))
            .expect_err("wrong token should be rejected");
        assert_eq!(err.code(), tonic::Code::Unauthenticated);
    }
}
