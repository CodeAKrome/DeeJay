#!/usr/bin/env python3
"""
MongoDB Regex Search CLI Tool (with env var auth)

Searches a MongoDB collection for documents where string fields match a regex.

Authentication:
- If MONGO_USER and MONGO_PASS environment variables are set → uses them.
- If not set → connects without authentication (useful for local unsecured instances).

Export variables before running:
  export MONGO_USER="myuser"
  export MONGO_PASS="mypassword"
"""

import os
import sys
from typing import List, Optional

import fire
from pymongo import MongoClient
from pymongo.errors import ConnectionFailure, OperationFailure
import re
from tqdm import tqdm


class MongoRegexSearch:
    def __init__(
        self,
        host: str = "localhost",
        port: int = 27017,
        auth_source: str = "admin",
        unicode_decode_error_handler: str = "strict",
    ):
        """
        Initialize MongoDB connection using MONGO_USER and MONGO_PASS env vars if available.
        
        Args:
            unicode_decode_error_handler: How to handle unicode decode errors.
                Options: 'strict' (default, raise errors), 'ignore' (skip bad chars), 
                'replace' (replace with �)
        """
        username = os.getenv("MONGO_USER")
        password = os.getenv("MONGO_PASS")

        if username and password:
            connection_string = f"mongodb://{username}:{password}@{host}:{port}/?authSource={auth_source}"
            print("Connecting with provided MONGO_USER and MONGO_PASS credentials.")
        else:
            connection_string = f"mongodb://{host}:{port}/"
            print("No MONGO_USER/MONGO_PASS found → connecting without authentication.")

        try:
            self.client = MongoClient(
                connection_string,
                unicode_decode_error_handler=unicode_decode_error_handler
            )
            # Test the connection
            self.client.admin.command("ping")
            print("Connected to MongoDB successfully.")
        except ConnectionFailure as e:
            print(f"Failed to connect to MongoDB: {e}")
            sys.exit(1)
        except Exception as e:
            print(f"Connection error: {e}")
            sys.exit(1)

    def _is_string_field(self, value) -> bool:
        return isinstance(value, str)

    def _build_regex(self, pattern: str, ignore_case: bool = False, regex: bool = False):
        flags = re.IGNORECASE if ignore_case else 0
        if regex:
            return re.compile(pattern, flags)
        else:
            return re.compile(re.escape(pattern), flags)

    def _parse_field_list(self, fields):
        """Parse fields parameter which can be string or tuple/list from Fire."""
        if not fields:
            return None
        if isinstance(fields, str):
            return [f.strip() for f in fields.split(",")]
        else:
            # Already a tuple or list from Fire
            return [f.strip() for f in fields]

    def find_corrupted(
        self,
        database: str,
        collection: str,
        fields: Optional[str] = None,
        limit: int = 0,
        projection: Optional[str] = None,
        show_progress: bool = True,
    ):
        """
        Find documents with Unicode replacement characters (�) indicating corrupted UTF-8 data.

        Args:
            database: Database name
            collection: Collection name
            fields: Comma-separated field names to check – if omitted, checks all string fields
            limit: Max results to show (0 = no limit)
            projection: Comma-separated fields to display
            show_progress: Show progress bar (default: True)
        """
        db = self.client[database]
        coll = db[collection]

        replacement_char = '\ufffd'  # Unicode replacement character �
        
        target_fields = self._parse_field_list(fields)

        # Build query if specific fields are provided
        if target_fields:
            or_conditions = [{field: {"$regex": replacement_char}} for field in target_fields]
            query = {"$or": or_conditions}
        else:
            query = {}  # We'll filter manually in Python

        try:
            cursor = coll.find(query)

            # Optional projection
            if projection:
                proj_fields = self._parse_field_list(projection)
                proj_dict = {f: 1 for f in proj_fields}
                proj_dict["_id"] = 1
                cursor = cursor.project(proj_dict)

            # For manual filtering (no target_fields), don't limit the cursor at all
            if target_fields and limit > 0:
                cursor = cursor.limit(limit)

            results = []
            
            # Create progress bar
            if target_fields:
                total_count = coll.count_documents(query)
                if limit > 0:
                    total_count = min(total_count, limit)
                pbar = tqdm(total=total_count, desc="Scanning documents", disable=not show_progress)
            else:
                pbar = tqdm(desc="Scanning documents", disable=not show_progress)
            
            for doc in cursor:
                if target_fields:
                    results.append(doc)
                    pbar.update(1)
                else:
                    # Manual check across all string fields
                    if any(
                        self._is_string_field(value) and replacement_char in value
                        for value in doc.values()
                    ):
                        results.append(doc)
                        if limit > 0 and len(results) >= limit:
                            pbar.update(1)
                            break
                    pbar.update(1)
            
            pbar.close()

            print(f"\nFound {len(results)} document(s) with corrupted UTF-8 in {database}.{collection}:\n")
            for i, doc in enumerate(results, 1):
                print(f"--- Result {i} ---")
                for k, v in doc.items():
                    if isinstance(v, str):
                        if replacement_char in v:
                            # Highlight corrupted fields
                            if len(v) > 300:
                                print(f"{k}: {v[:297]}... [CORRUPTED - contains �]")
                            else:
                                print(f"{k}: {v} [CORRUPTED - contains �]")
                        else:
                            if len(v) > 300:
                                print(f"{k}: {v[:297]}...")
                            else:
                                print(f"{k}: {v}")
                    else:
                        print(f"{k}: {v}")
                print()

        except OperationFailure as e:
            print(f"Query failed (auth/permission issue?): {e}")
        except Exception as e:
            print(f"Error during search: {e}")

    def find_utf8_errors(
        self,
        database: str,
        collection: str,
        fields: Optional[str] = None,
        limit: int = 0,
        projection: Optional[str] = None,
        show_progress: bool = True,
        fix: bool = False,
        replacement: str = "",
        dry_run: bool = False,
    ):
        """
        Find documents that cause UTF-8 decode errors when read with strict validation.
        
        This command temporarily reconnects with strict UTF-8 validation to identify
        documents that have invalid UTF-8 sequences (before they get replaced with �).

        Args:
            database: Database name
            collection: Collection name
            fields: Comma-separated field names to check – if omitted, checks all string fields
            limit: Max results to show (0 = no limit)
            projection: Comma-separated fields to display
            show_progress: Show progress bar (default: True)
            fix: Attempt to fix corrupted fields by removing invalid chars or replacing with replacement string
            replacement: String to use when invalid chars can't be removed (default: empty string)
            dry_run: Show what would be changed without actually updating the database
        """
        # Create a temporary strict client to test for UTF-8 errors
        username = os.getenv("MONGO_USER")
        password = os.getenv("MONGO_PASS")
        
        if username and password:
            connection_string = f"mongodb://{username}:{password}@localhost:27017/?authSource=admin"
        else:
            connection_string = f"mongodb://localhost:27017/"
        
        try:
            strict_client = MongoClient(
                connection_string,
                unicode_decode_error_handler='strict'
            )
            strict_db = strict_client[database]
            strict_coll = strict_db[collection]
            
            # Create a lenient client specifically for reading corrupted data
            lenient_client = MongoClient(
                connection_string,
                unicode_decode_error_handler='replace'
            )
            lenient_db = lenient_client[database]
            lenient_coll = lenient_db[collection]
            
            # Use main client for updates (it should already be lenient from __init__)
            db = self.client[database]
            coll = db[collection]
            
            target_fields = self._parse_field_list(fields)
            
            # Get all document IDs to test (use lenient client)
            id_cursor = lenient_coll.find({}, {"_id": 1})
            if limit > 0:
                id_cursor = id_cursor.limit(limit)
            
            doc_ids = [doc["_id"] for doc in id_cursor]
            
            results = []
            error_docs = []
            
            pbar = tqdm(total=len(doc_ids), desc="Testing documents for UTF-8 errors", disable=not show_progress)
            
            for doc_id in doc_ids:
                try:
                    # Try to read with strict validation
                    doc = strict_coll.find_one({"_id": doc_id})
                    
                    # If we get here, force iteration over all fields to trigger any errors
                    if doc:
                        if target_fields:
                            # Only check specified fields
                            for field in target_fields:
                                if field in doc and isinstance(doc[field], str):
                                    # This will trigger decode if there's invalid UTF-8
                                    _ = len(doc[field])
                        else:
                            # Check all string fields
                            for key, value in doc.items():
                                if isinstance(value, str):
                                    # This will trigger decode if there's invalid UTF-8
                                    _ = len(value)
                    pbar.update(1)
                    
                except (UnicodeDecodeError, Exception) as e:
                    # UTF-8 decode error found - fetch full doc with lenient client
                    try:
                        full_doc = lenient_coll.find_one({"_id": doc_id})
                        
                        # Find which fields have errors and prepare fixes
                        corrupted_fields = {}
                        if full_doc:
                            check_fields = target_fields if target_fields else [k for k in full_doc.keys() if k != '_id']
                            for field in check_fields:
                                if field in full_doc and isinstance(full_doc[field], str):
                                    original = full_doc[field]
                                    # Check if this field has replacement characters
                                    if '\ufffd' in original:
                                        # Try to clean the field by removing replacement chars
                                        cleaned = original.replace('\ufffd', '')
                                        
                                        if not cleaned.strip() and replacement:
                                            # If cleaning left nothing, use replacement
                                            corrupted_fields[field] = {
                                                'original': original,
                                                'cleaned': replacement,
                                                'method': 'replaced_with_custom'
                                            }
                                        elif not cleaned.strip():
                                            # If no replacement specified and field is empty, skip
                                            corrupted_fields[field] = {
                                                'original': original,
                                                'cleaned': '',
                                                'method': 'removed_invalid_chars'
                                            }
                                        else:
                                            # Use cleaned version
                                            corrupted_fields[field] = {
                                                'original': original,
                                                'cleaned': cleaned,
                                                'method': 'removed_invalid_chars'
                                            }
                        
                        error_docs.append({
                            "_id": doc_id,
                            "error": str(e),
                            "doc": full_doc,
                            "corrupted_fields": corrupted_fields
                        })
                    except Exception as fetch_error:
                        error_docs.append({
                            "_id": doc_id,
                            "error": f"{str(e)} (Could not fetch with lenient client: {str(fetch_error)})",
                            "doc": {"_id": doc_id},
                            "corrupted_fields": {}
                        })
                    pbar.update(1)
            
            pbar.close()
            strict_client.close()
            lenient_client.close()
            
            if fix and not dry_run:
                print(f"\n{'=' * 80}")
                print("FIXING CORRUPTED DOCUMENTS")
                print(f"{'=' * 80}\n")
            elif fix and dry_run:
                print(f"\n{'=' * 80}")
                print("DRY RUN - Showing proposed changes (no actual updates)")
                print(f"{'=' * 80}\n")
            
            print(f"\nFound {len(error_docs)} document(s) with UTF-8 errors in {database}.{collection}:\n")
            
            fixed_count = 0
            for i, item in enumerate(error_docs, 1):
                doc = item["doc"]
                corrupted_fields = item.get("corrupted_fields", {})
                
                print(f"--- Result {i} ---")
                print(f"Error: {item['error']}")
                print(f"Document ID: {doc.get('_id')}")
                
                if corrupted_fields:
                    print(f"\nCorrupted fields found: {len(corrupted_fields)}")
                    for field, info in corrupted_fields.items():
                        print(f"\n  Field: {field}")
                        print(f"  Method: {info['method']}")
                        orig_preview = info['original'][:100] + "..." if len(info['original']) > 100 else info['original']
                        clean_preview = info['cleaned'][:100] + "..." if len(info['cleaned']) > 100 else info['cleaned']
                        print(f"  Original: {repr(orig_preview)}")
                        print(f"  Cleaned:  {repr(clean_preview)}")
                    
                    # Perform the fix if requested
                    if fix:
                        update_dict = {field: info['cleaned'] for field, info in corrupted_fields.items()}
                        
                        if dry_run:
                            print(f"\n  [DRY RUN] Would update: {update_dict}")
                        else:
                            try:
                                result = coll.update_one(
                                    {"_id": doc["_id"]},
                                    {"$set": update_dict}
                                )
                                if result.modified_count > 0:
                                    print(f"\n  ✓ FIXED: Updated {len(update_dict)} field(s)")
                                    fixed_count += 1
                                else:
                                    print(f"\n  ✗ Update failed or no changes made")
                            except Exception as update_error:
                                print(f"\n  ✗ Error updating document: {update_error}")
                else:
                    print("\n  (No corrupted fields identified in target fields)")
                
                # Show full document if no projection specified
                if not projection and doc.get('_id'):
                    print("\n  Full document:")
                    for k, v in doc.items():
                        if isinstance(v, str) and len(v) > 300:
                            print(f"    {k}: {v[:297]}...")
                        else:
                            print(f"    {k}: {v}")
                else:
                    proj_fields = self._parse_field_list(projection)
                    filtered_doc = {k: v for k, v in doc.items() if k in proj_fields or k == "_id"}
                    print("\n  Selected fields:")
                    for k, v in filtered_doc.items():
                        if isinstance(v, str) and len(v) > 300:
                            print(f"    {k}: {v[:297]}...")
                        else:
                            print(f"    {k}: {v}")
                
                print()
            
            if fix and not dry_run:
                print(f"\n{'=' * 80}")
                print(f"SUMMARY: Fixed {fixed_count} out of {len(error_docs)} documents")
                print(f"{'=' * 80}\n")
            elif fix and dry_run:
                print(f"\n{'=' * 80}")
                print(f"DRY RUN COMPLETE: Would fix {len([d for d in error_docs if d.get('corrupted_fields')])} documents")
                print(f"{'=' * 80}\n")

        except Exception as e:
            print(f"Error during UTF-8 validation scan: {e}")

    def search(
        self,
        database: str,
        collection: str,
        pattern: str,
        fields: Optional[str] = None,
        ignore_case: bool = True,
        regex: bool = False,
        limit: int = 50,
        projection: Optional[str] = None,
        show_progress: bool = True,
    ):
        """
        Search documents matching a regex pattern in specified or all string fields.

        Args:
            database: Database name
            collection: Collection name
            pattern: Search pattern
            fields: Comma-separated field names (e.g. "artist,album_artist") – if omitted, searches all string fields
            ignore_case: Case-insensitive (default: True)
            regex: Treat pattern as raw regex instead of escaping special chars
            limit: Max results to show (0 = no limit)
            projection: Comma-separated fields to display
            show_progress: Show progress bar (default: True)
        """
        db = self.client[database]
        coll = db[collection]

        regex_pattern = self._build_regex(pattern, ignore_case=ignore_case, regex=regex)
        target_fields = self._parse_field_list(fields)

        # Build query if specific fields are provided
        if target_fields:
            or_conditions = [{field: {"$regex": regex_pattern}} for field in target_fields]
            query = {"$or": or_conditions}
        else:
            query = {}  # We'll filter manually in Python

        try:
            cursor = coll.find(query)

            # Optional projection
            if projection:
                proj_fields = self._parse_field_list(projection)
                proj_dict = {f: 1 for f in proj_fields}
                proj_dict["_id"] = 1
                cursor = cursor.project(proj_dict)

            # For manual filtering (no target_fields), don't limit the cursor at all
            if target_fields and limit > 0:
                cursor = cursor.limit(limit)

            results = []
            
            # Create progress bar
            if target_fields:
                total_count = coll.count_documents(query)
                if limit > 0:
                    total_count = min(total_count, limit)
                pbar = tqdm(total=total_count, desc="Searching documents", disable=not show_progress)
            else:
                pbar = tqdm(desc="Scanning documents", disable=not show_progress)
            
            for doc in cursor:
                if target_fields:
                    results.append(doc)
                    pbar.update(1)
                else:
                    # Manual check across all string fields
                    matched = any(
                        self._is_string_field(value) and regex_pattern.search(value)
                        for value in doc.values()
                    )
                    if matched:
                        results.append(doc)
                        if limit > 0 and len(results) >= limit:
                            pbar.update(1)
                            break
                    pbar.update(1)
            
            pbar.close()

            print(f"\nFound {len(results)} matching document(s) in {database}.{collection}:\n")
            for i, doc in enumerate(results, 1):
                print(f"--- Result {i} ---")
                for k, v in doc.items():
                    if isinstance(v, str) and len(v) > 300:
                        print(f"{k}: {v[:297]}...")
                    else:
                        print(f"{k}: {v}")
                print()

        except OperationFailure as e:
            print(f"Query failed (auth/permission issue?): {e}")
        except Exception as e:
            print(f"Error during search: {e}")

    def close(self):
        self.client.close()
        print("Connection closed.")


def main():
    fire.Fire(MongoRegexSearch)


if __name__ == "__main__":
    main()