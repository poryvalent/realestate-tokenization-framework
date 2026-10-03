import { Link, useParams } from 'react-router-dom';
import { Unbuilt } from '../components/ui';

export default function Documents() {
  const { id } = useParams();
  return (
    <>
      <p style={{ marginTop: 24 }}><Link to={`/offers/${id}`}>← Offer</Link></p>
      <h1>Offer documents</h1>
      <Unbuilt
        title="Document register publishes at freeze"
        body="Pinned offer documents with the digest anchored on-chain. The commitment is the document digest, not the locator: verification is fetch bytes, hash them, compare."
        endpoint={`GET /offers/${id}/documents`}
      />
    </>
  );
}
