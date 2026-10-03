import { Link, useParams } from 'react-router-dom';
import { motion } from 'framer-motion';
import { ArrowLeft, FileLock2 } from 'lucide-react';
import { Unbuilt, Reveal } from '../components/ui';
import { Io } from '../components/motion';

export default function Documents() {
  const { id } = useParams();
  return (
    <>
      <p className="mt-6">
        <Link
          to={`/offers/${id}`}
          className="inline-flex items-center gap-2 text-sm text-white/60 transition hover:text-white"
        >
          <ArrowLeft className="h-4 w-4" /> Offer
        </Link>
      </p>
      <Reveal
        title={
          <>
            Offer documents<span className="text-brand-blue">.</span>
          </>
        }
        lede="Pinned offer documents with the digest anchored on-chain. The commitment is the document digest, not the locator: verification is fetch bytes, hash them, compare."
      />
      <motion.div
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.5, delay: 0.15 }}
        className="mt-8 flex items-center gap-3 text-xs font-medium uppercase tracking-[0.25em] text-white/40"
      >
        <FileLock2 className="h-4 w-4 text-brand-blue" />
        Document register · publishes at freeze
      </motion.div>
      <Io delay={120} className="mt-4">
        <Unbuilt
          title="Document register publishes at freeze"
          body="Pinned offer documents with the digest anchored on-chain. The commitment is the document digest, not the locator: verification is fetch bytes, hash them, compare."
          endpoint={`GET /offers/${id}/documents`}
        />
      </Io>
    </>
  );
}
